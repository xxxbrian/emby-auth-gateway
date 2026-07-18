package gateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errMediaBufferCopyState = errors.New("invalid media buffer copy state")
var errMediaBufferCopyInvariant = errors.New("media buffer copy ownership invariant failed")

type mediaBufferCopyHooks struct {
	onOptionalWait   func()
	onPublished      func(terminal bool)
	onBeforeDequeue  func(terminal bool)
	injectReleaseErr error
	injectCancelErr  error
}

type mediaBufferCopyBuffer struct {
	data  []byte
	base  bool
	lease mediaBufferLease
}

type mediaBufferCopyEvent struct {
	buffer    mediaBufferCopyBuffer
	length    int
	terminal  bool
	direction string
	err       error
}

type mediaBufferProducerResult struct {
	bytesRead int64
	direction string
	err       error
	invariant error
}

type mediaBufferCopyQueue struct {
	mu      sync.Mutex
	events  []mediaBufferCopyEvent
	head    int
	notify  chan struct{}
	closing bool
}

func copyBufferedMediaBody(ctx context.Context, dst io.Writer, src io.Reader, source io.Closer, base []byte, request *mediaBufferRequest, expectedLength int64) mediaCopyResult {
	return copyBufferedMediaBodyWithHooks(ctx, dst, src, source, base, request, expectedLength, nil)
}

func copyBufferedMediaBodyWithHooks(ctx context.Context, dst io.Writer, src io.Reader, source io.Closer, base []byte, request *mediaBufferRequest, expectedLength int64, hooks *mediaBufferCopyHooks) mediaCopyResult {
	started := time.Now()
	result := mediaCopyResult{}
	finish := func(direction string, err error) mediaCopyResult {
		result.Direction = direction
		result.Duration = time.Since(started)
		result.Err = err
		return result
	}
	if ctx == nil || dst == nil || src == nil || source == nil || request == nil || len(base) != mediaCopyBufferSize {
		return finish(mediaDirectionUpstream, errMediaBufferCopyState)
	}

	producerCtx, cancelProducer := context.WithCancel(ctx)
	queue := &mediaBufferCopyQueue{notify: make(chan struct{}, 1)}
	baseReady := make(chan []byte, 1)
	producerDone := make(chan mediaBufferProducerResult, 1)
	go produceBufferedMedia(producerCtx, src, request, mediaBufferCopyBuffer{data: base, base: true}, baseReady, queue, producerDone, expectedLength, hooks)

	type selectedResult struct {
		direction string
		err       error
	}
	selected := selectedResult{}
	for {
		event, ok := queue.next(ctx, hooks)
		if !ok {
			selected.err = ctx.Err()
			if selected.err == nil {
				selected.direction = mediaDirectionUpstream
				selected.err = errMediaBufferCopyState
			}
			break
		}
		if event.terminal {
			selected.direction = event.direction
			selected.err = event.err
			break
		}

		written, writeErr := dst.Write(event.buffer.data[:event.length])
		if written < 0 || written > event.length {
			selected.direction = mediaDirectionDownstream
			selected.err = errors.New("invalid downstream media write")
		} else {
			result.BytesWritten += int64(written)
			if writeErr != nil {
				selected.direction = mediaDirectionDownstream
				selected.err = writeErr
			} else if written < event.length {
				selected.direction = mediaDirectionDownstream
				selected.err = io.ErrShortWrite
			}
		}
		if releaseErr := releaseBufferedCopyBuffer(request, baseReady, event.buffer, hooks); releaseErr != nil {
			if selected.err == nil {
				selected.direction = mediaDirectionUpstream
				selected.err = releaseErr
			} else {
				selected.err = errors.Join(selected.err, releaseErr)
			}
		}
		if selected.err != nil {
			break
		}
	}

	queued := queue.close()
	cancelProducer()
	var invariantErr error
	if cancelErr := cancelBufferedOptionalRequest(request, hooks); cancelErr != nil {
		invariantErr = errors.Join(invariantErr, cancelErr)
	}
	_ = source.Close()
	producerResult := <-producerDone
	invariantErr = errors.Join(invariantErr, producerResult.invariant)
	for _, event := range append(queued, queue.drain()...) {
		if event.terminal {
			continue
		}
		if releaseErr := releaseBufferedCopyBuffer(request, baseReady, event.buffer, hooks); releaseErr != nil {
			invariantErr = errors.Join(invariantErr, releaseErr)
		}
	}
	result.BytesRead = producerResult.bytesRead
	if invariantErr != nil {
		if selected.err == nil {
			selected.direction = mediaDirectionUpstream
			selected.err = invariantErr
		} else {
			selected.err = errors.Join(selected.err, invariantErr)
		}
	}

	if selected.err != nil || selected.direction != "" {
		return finish(selected.direction, selected.err)
	}
	return finish("", nil)
}

func produceBufferedMedia(ctx context.Context, src io.Reader, request *mediaBufferRequest, current mediaBufferCopyBuffer, baseReady chan []byte, queue *mediaBufferCopyQueue, done chan<- mediaBufferProducerResult, expectedLength int64, hooks *mediaBufferCopyHooks) {
	result := mediaBufferProducerResult{}
	defer func() { done <- result }()
	emptyReads := 0

	for {
		n, readErr := src.Read(current.data[:mediaCopyBufferSize])
		if n < 0 || n > mediaCopyBufferSize {
			result.invariant = errors.Join(result.invariant, releaseProducerBuffer(request, current, hooks))
			result.direction = mediaDirectionUpstream
			result.err = errors.New("invalid upstream media read")
			publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: result.err}, hooks)
			return
		}
		if n > 0 {
			emptyReads = 0
			result.bytesRead += int64(n)
			writeLength := n
			lengthMismatch := false
			if expectedLength >= 0 && result.bytesRead > expectedLength {
				writeLength = n - int(result.bytesRead-expectedLength)
				lengthMismatch = true
			}
			if writeLength > 0 {
				if !publishBufferedMediaEvent(queue, mediaBufferCopyEvent{buffer: current, length: writeLength}, hooks) {
					result.invariant = errors.Join(result.invariant, releaseProducerBuffer(request, current, hooks))
					result.err = ctx.Err()
					return
				}
				current = mediaBufferCopyBuffer{}
			} else {
				result.invariant = errors.Join(result.invariant, releaseProducerBuffer(request, current, hooks))
				current = mediaBufferCopyBuffer{}
			}
			if lengthMismatch {
				result.direction = mediaDirectionUpstream
				result.err = errMediaLengthMismatch
				publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: result.err}, hooks)
				return
			}
			if readErr != nil {
				result.direction, result.err = classifyBufferedReadTerminal(readErr, result.bytesRead, expectedLength)
				publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: result.err}, hooks)
				return
			}
			next, err := acquireBufferedCopyBuffer(ctx, request, baseReady, hooks)
			if err != nil {
				if errors.Is(err, errMediaBufferCopyInvariant) {
					result.invariant = errors.Join(result.invariant, err)
				}
				if ctx.Err() == nil {
					result.direction = mediaDirectionUpstream
					publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: err}, hooks)
				}
				result.err = err
				return
			}
			current = next
			continue
		}

		if readErr == nil {
			emptyReads++
			if emptyReads < 100 {
				continue
			}
			result.invariant = errors.Join(result.invariant, releaseProducerBuffer(request, current, hooks))
			result.direction = mediaDirectionUpstream
			result.err = io.ErrNoProgress
			publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: result.err}, hooks)
			return
		}

		result.invariant = errors.Join(result.invariant, releaseProducerBuffer(request, current, hooks))
		result.direction, result.err = classifyBufferedReadTerminal(readErr, result.bytesRead, expectedLength)
		publishBufferedMediaEvent(queue, mediaBufferCopyEvent{terminal: true, direction: result.direction, err: result.err}, hooks)
		return
	}
}

func publishBufferedMediaEvent(queue *mediaBufferCopyQueue, event mediaBufferCopyEvent, hooks *mediaBufferCopyHooks) bool {
	if !queue.publish(event) {
		return false
	}
	if hooks != nil && hooks.onPublished != nil {
		hooks.onPublished(event.terminal)
	}
	return true
}

func classifyBufferedReadTerminal(readErr error, bytesRead, expectedLength int64) (string, error) {
	if readErr != io.EOF {
		return mediaDirectionUpstream, readErr
	}
	if expectedLength >= 0 && bytesRead < expectedLength {
		return mediaDirectionUpstream, io.ErrUnexpectedEOF
	}
	return "", nil
}

func acquireBufferedCopyBuffer(ctx context.Context, request *mediaBufferRequest, baseReady chan []byte, hooks *mediaBufferCopyHooks) (mediaBufferCopyBuffer, error) {
	select {
	case base := <-baseReady:
		return mediaBufferCopyBuffer{data: base, base: true}, nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return mediaBufferCopyBuffer{}, err
	}
	notify, err := request.requestOptional()
	if err != nil {
		return mediaBufferCopyBuffer{}, mediaBufferCopyInvariantError(err)
	}
	select {
	case base := <-baseReady:
		if cancelErr := cancelBufferedOptionalRequest(request, hooks); cancelErr != nil {
			return mediaBufferCopyBuffer{}, cancelErr
		}
		return mediaBufferCopyBuffer{data: base, base: true}, nil
	default:
	}
	select {
	case <-notify:
		lease, acceptErr := request.acceptOptional()
		if acceptErr != nil {
			return mediaBufferCopyBuffer{}, mediaBufferCopyInvariantError(acceptErr)
		}
		return mediaBufferCopyBuffer{data: lease.bytes(), lease: lease}, nil
	default:
	}
	if hooks != nil && hooks.onOptionalWait != nil {
		hooks.onOptionalWait()
	}
	select {
	case <-ctx.Done():
		return mediaBufferCopyBuffer{}, errors.Join(ctx.Err(), cancelBufferedOptionalRequest(request, hooks))
	case base := <-baseReady:
		if cancelErr := cancelBufferedOptionalRequest(request, hooks); cancelErr != nil {
			return mediaBufferCopyBuffer{}, cancelErr
		}
		return mediaBufferCopyBuffer{data: base, base: true}, nil
	case _, ok := <-notify:
		if !ok {
			return mediaBufferCopyBuffer{}, mediaBufferCopyInvariantError(errMediaBufferClosed)
		}
		lease, acceptErr := request.acceptOptional()
		if acceptErr != nil {
			return mediaBufferCopyBuffer{}, mediaBufferCopyInvariantError(acceptErr)
		}
		return mediaBufferCopyBuffer{data: lease.bytes(), lease: lease}, nil
	}
}

func releaseProducerBuffer(request *mediaBufferRequest, buffer mediaBufferCopyBuffer, hooks *mediaBufferCopyHooks) error {
	if len(buffer.data) == 0 || buffer.base {
		return nil
	}
	return releaseBufferedOptionalLease(request, buffer.lease, hooks)
}

func releaseBufferedCopyBuffer(request *mediaBufferRequest, baseReady chan []byte, buffer mediaBufferCopyBuffer, hooks *mediaBufferCopyHooks) error {
	if buffer.base {
		select {
		case baseReady <- buffer.data:
			return nil
		default:
			return mediaBufferCopyInvariantError(errMediaBufferCopyState)
		}
	}
	return releaseBufferedOptionalLease(request, buffer.lease, hooks)
}

func releaseBufferedOptionalLease(request *mediaBufferRequest, lease mediaBufferLease, hooks *mediaBufferCopyHooks) error {
	err := request.releaseOptional(lease)
	if err == nil && hooks != nil && hooks.injectReleaseErr != nil {
		err = hooks.injectReleaseErr
	}
	return mediaBufferCopyInvariantError(err)
}

func cancelBufferedOptionalRequest(request *mediaBufferRequest, hooks *mediaBufferCopyHooks) error {
	err := request.cancelOptionalRequest()
	if err == nil && hooks != nil && hooks.injectCancelErr != nil {
		err = hooks.injectCancelErr
	}
	return mediaBufferCopyInvariantError(err)
}

func mediaBufferCopyInvariantError(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(errMediaBufferCopyInvariant, err)
}

func (q *mediaBufferCopyQueue) publish(event mediaBufferCopyEvent) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing {
		return false
	}
	q.events = append(q.events, event)
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

func (q *mediaBufferCopyQueue) next(ctx context.Context, hooks *mediaBufferCopyHooks) (mediaBufferCopyEvent, bool) {
	for {
		q.mu.Lock()
		if q.head < len(q.events) {
			event := q.events[q.head]
			if hooks != nil && hooks.onBeforeDequeue != nil {
				hooks.onBeforeDequeue(event.terminal)
			}
			if ctx.Err() != nil {
				q.mu.Unlock()
				return mediaBufferCopyEvent{}, false
			}
			q.events[q.head] = mediaBufferCopyEvent{}
			q.head++
			q.compactLocked()
			q.mu.Unlock()
			return event, true
		}
		if ctx.Err() != nil {
			q.mu.Unlock()
			return mediaBufferCopyEvent{}, false
		}
		closing := q.closing
		q.mu.Unlock()
		if closing {
			return mediaBufferCopyEvent{}, false
		}
		select {
		case <-ctx.Done():
			return mediaBufferCopyEvent{}, false
		case <-q.notify:
		}
	}
}

func (q *mediaBufferCopyQueue) close() []mediaBufferCopyEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closing = true
	events := q.takeAllLocked()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return events
}

func (q *mediaBufferCopyQueue) drain() []mediaBufferCopyEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.takeAllLocked()
}

func (q *mediaBufferCopyQueue) compactLocked() {
	if q.head == len(q.events) {
		q.events = nil
		q.head = 0
		return
	}
	if q.head < 64 || q.head*2 < len(q.events) {
		return
	}
	remaining := copy(q.events, q.events[q.head:])
	for index := remaining; index < len(q.events); index++ {
		q.events[index] = mediaBufferCopyEvent{}
	}
	q.events = q.events[:remaining]
	q.head = 0
}

func (q *mediaBufferCopyQueue) takeAllLocked() []mediaBufferCopyEvent {
	if q.head >= len(q.events) {
		q.events = nil
		q.head = 0
		return nil
	}
	events := q.events[q.head:]
	for index := 0; index < q.head; index++ {
		q.events[index] = mediaBufferCopyEvent{}
	}
	q.events = nil
	q.head = 0
	return events
}
