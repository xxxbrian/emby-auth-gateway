package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaBufferCopyMatchesSynchronousReadSemantics(t *testing.T) {
	readErr := errors.New("upstream failure")
	tests := []struct {
		name     string
		newRead  func() io.Reader
		expected int64
	}{
		{name: "exact known length", newRead: func() io.Reader {
			return &fastPathReader{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 3*mediaCopyBufferSize+17))}
		}, expected: 3*mediaCopyBufferSize + 17},
		{name: "unknown length", newRead: func() io.Reader { return bytes.NewReader(bytes.Repeat([]byte("y"), mediaCopyBufferSize+9)) }, expected: -1},
		{name: "short body", newRead: func() io.Reader { return bytes.NewBufferString("short") }, expected: 10},
		{name: "overlength", newRead: func() io.Reader { return bytes.NewBufferString("media") }, expected: 3},
		{name: "declared empty", newRead: func() io.Reader { return bytes.NewBufferString("media") }, expected: 0},
		{name: "upstream error", newRead: func() io.Reader { return errorMediaReader{err: readErr} }, expected: -1},
		{name: "n plus eof", newRead: func() io.Reader { return &mediaBufferNErrorReader{data: []byte("media"), err: io.EOF} }, expected: 5},
		{name: "n plus error", newRead: func() io.Reader { return &mediaBufferNErrorReader{data: []byte("media"), err: readErr} }, expected: -1},
		{name: "empty reads", newRead: func() io.Reader { return &mediaBufferEmptyReader{} }, expected: -1},
		{name: "positive read resets empty count", newRead: func() io.Reader { return &mediaBufferResetEmptyReader{} }, expected: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncWriter := &fastPathWriter{}
			syncResult := copyMediaBody(syncWriter, tt.newRead(), tt.expected)
			bufferedWriter := &fastPathWriter{}
			bufferedResult, source := runMediaBufferCopy(t, context.Background(), bufferedWriter, tt.newRead(), tt.expected, 4*mediaBufferChunkSize, nil)
			assertMediaCopyParity(t, bufferedResult, syncResult)
			if !bytes.Equal(bufferedWriter.Bytes(), syncWriter.Bytes()) {
				t.Fatalf("buffered output length=%d sync=%d", bufferedWriter.Len(), syncWriter.Len())
			}
			assertMediaBufferSourceClosedOnce(t, source)
		})
	}
}

func TestMediaBufferCopyPreservesFullQuantumOverread(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 2*mediaCopyBufferSize)
	writer := &fastPathWriter{}
	result, _ := runMediaBufferCopy(t, context.Background(), writer, bytes.NewReader(payload), mediaCopyBufferSize, 2*mediaBufferChunkSize, nil)
	if !errors.Is(result.Err, errMediaLengthMismatch) || result.Direction != mediaDirectionUpstream || result.BytesRead != 2*mediaCopyBufferSize || result.BytesWritten != mediaCopyBufferSize || writer.Len() != mediaCopyBufferSize {
		t.Fatalf("result=%+v output=%d", result, writer.Len())
	}
}

func TestMediaBufferCopyMatchesSynchronousWriteSemantics(t *testing.T) {
	payload := bytes.Repeat([]byte("media"), 4000)
	shortSync := copyMediaBody(&shortMediaWriter{written: len(payload) / 2}, bytes.NewReader(payload), int64(len(payload)))
	shortWriter := &shortMediaWriter{written: len(payload) / 2}
	shortBuffered, _ := runMediaBufferCopy(t, context.Background(), shortWriter, bytes.NewReader(payload), int64(len(payload)), 2*mediaBufferChunkSize, nil)
	assertMediaCopyParity(t, shortBuffered, shortSync)

	writeErr := errors.New("write failed")
	partialSync := copyMediaBody(&partialErrorMediaWriter{err: writeErr}, bytes.NewBufferString("media"), 5)
	partialWriter := &partialErrorMediaWriter{err: writeErr}
	partialBuffered, _ := runMediaBufferCopy(t, context.Background(), partialWriter, bytes.NewBufferString("media"), 5, mediaBufferChunkSize, nil)
	assertMediaCopyParity(t, partialBuffered, partialSync)

	for _, invalidN := range []int{-1, 6} {
		invalidWriter := &mediaBufferInvalidWriter{n: invalidN, err: writeErr}
		invalid, _ := runMediaBufferCopy(t, context.Background(), invalidWriter, bytes.NewBufferString("media"), 5, mediaBufferChunkSize, nil)
		if invalid.Direction != mediaDirectionDownstream || invalid.BytesWritten != 0 || invalid.Err == nil || errors.Is(invalid.Err, writeErr) {
			t.Fatalf("n=%d invalid write result=%+v", invalidN, invalid)
		}
	}
}

func TestMediaBufferCopyRejectsInvalidReadCountsBeforeAccounting(t *testing.T) {
	for _, n := range []int{-1, mediaCopyBufferSize + 1} {
		result, _ := runMediaBufferCopy(t, context.Background(), io.Discard, mediaBufferInvalidReader{n: n}, -1, mediaBufferChunkSize, nil)
		if result.Direction != mediaDirectionUpstream || result.BytesRead != 0 || result.BytesWritten != 0 || result.Err == nil {
			t.Fatalf("n=%d result=%+v", n, result)
		}
	}
}

func TestMediaBufferCopyQueuedTerminalLosesToDownstreamFailure(t *testing.T) {
	upstreamErr := errors.New("queued upstream error")
	downstreamErr := errors.New("downstream failure")
	writer := &mediaBufferInvalidWriter{n: 0, err: downstreamErr}
	result, _ := runMediaBufferCopy(t, context.Background(), writer, &mediaBufferNErrorReader{data: []byte("media"), err: upstreamErr}, -1, mediaBufferChunkSize, nil)
	if !errors.Is(result.Err, downstreamErr) || result.Direction != mediaDirectionDownstream || result.BytesRead != 5 || result.BytesWritten != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestMediaBufferCopyCancellationBeforeTerminalDequeue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	terminalPublished := make(chan struct{})
	beforeDequeue := make(chan struct{})
	allowDequeue := make(chan struct{})
	var terminalOnce sync.Once
	var dequeueOnce sync.Once
	hooks := &mediaBufferCopyHooks{onPublished: func(terminal bool) {
		if terminal {
			terminalOnce.Do(func() { close(terminalPublished) })
		}
	}, onBeforeDequeue: func(terminal bool) {
		if terminal {
			dequeueOnce.Do(func() { close(beforeDequeue) })
			<-allowDequeue
		}
	}}
	writer := &mediaBufferTerminalBarrierWriter{terminalPublished: terminalPublished}
	resultCh, _, request := startMediaBufferCopy(t, ctx, writer, &mediaBufferNErrorReader{data: []byte("media"), err: io.EOF}, 5, mediaBufferChunkSize, hooks)
	awaitMediaBufferSignal(t, beforeDequeue)
	cancel()
	close(allowDequeue)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) || result.Direction != "" || result.BytesRead != 5 || result.BytesWritten != 5 {
		t.Fatalf("result=%+v", result)
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyOwnershipFailuresAreSurfaced(t *testing.T) {
	t.Run("cancel cleanup", func(t *testing.T) {
		injected := errors.New("injected cancel failure")
		hooks := &mediaBufferCopyHooks{injectCancelErr: injected}
		result, _ := runMediaBufferCopy(t, context.Background(), io.Discard, bytes.NewBufferString("media"), 5, mediaBufferChunkSize, hooks)
		if result.Direction != mediaDirectionUpstream || !errors.Is(result.Err, errMediaBufferCopyInvariant) || !errors.Is(result.Err, injected) {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("producer optional release", func(t *testing.T) {
		injected := errors.New("injected producer release failure")
		reader := &mediaBufferInvalidOptionalReader{optionalRead: make(chan struct{})}
		writer := newMediaBufferBlockingWriter(nil)
		hooks := &mediaBufferCopyHooks{injectReleaseErr: injected}
		resultCh, _, request := startMediaBufferCopy(t, context.Background(), writer, reader, -1, mediaBufferChunkSize, hooks)
		awaitMediaBufferSignal(t, writer.started)
		awaitMediaBufferSignal(t, reader.optionalRead)
		close(writer.release)
		result := awaitMediaBufferResult(t, resultCh)
		if result.Direction != mediaDirectionUpstream || !errors.Is(result.Err, errMediaBufferCopyInvariant) || !errors.Is(result.Err, injected) {
			t.Fatalf("result=%+v", result)
		}
		closeMediaBufferCopyRequests(t, request)
	})

	t.Run("consumed optional downstream failure", func(t *testing.T) {
		downstreamErr := errors.New("optional downstream failure")
		injected := errors.New("injected consumed release failure")
		reader := &mediaBufferReadCountReader{data: bytes.Repeat([]byte("o"), 2*mediaCopyBufferSize), reached: make(chan struct{})}
		writer := newMediaBufferFailSecondWriter(downstreamErr)
		hooks := &mediaBufferCopyHooks{injectReleaseErr: injected}
		resultCh, _, request := startMediaBufferCopy(t, context.Background(), writer, reader, -1, mediaBufferChunkSize, hooks)
		awaitMediaBufferSignal(t, writer.started)
		awaitMediaBufferSignal(t, reader.reached)
		close(writer.releaseFirst)
		result := awaitMediaBufferResult(t, resultCh)
		if result.Direction != mediaDirectionDownstream || !errors.Is(result.Err, downstreamErr) || !errors.Is(result.Err, errMediaBufferCopyInvariant) || !errors.Is(result.Err, injected) {
			t.Fatalf("result=%+v", result)
		}
		if got := request.snapshot(); got.Owned != 0 || got.Pending || got.Requesting {
			t.Fatalf("request after simultaneous failure=%+v", got)
		}
		closeMediaBufferCopyRequests(t, request)
	})
}

func TestMediaBufferCopyQueuePreservesLargeFIFOOrder(t *testing.T) {
	queue := &mediaBufferCopyQueue{notify: make(chan struct{}, 1)}
	const eventCount = 10000
	for index := 0; index < eventCount; index++ {
		if !queue.publish(mediaBufferCopyEvent{length: index}) {
			t.Fatal("queue closed during publish")
		}
	}
	for index := 0; index < eventCount; index++ {
		event, ok := queue.next(context.Background(), nil)
		if !ok || event.length != index {
			t.Fatalf("index=%d event=%+v ok=%v", index, event, ok)
		}
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.events) != 0 || queue.head != 0 {
		t.Fatalf("queue retained released references: len=%d head=%d", len(queue.events), queue.head)
	}
}

func TestMediaBufferCopyCancellationWhileWaitingForCapacity(t *testing.T) {
	buffer := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	blocker := buffer.register()
	blockerLeases := []mediaBufferLease{acceptMediaBufferCopyLease(t, blocker), acceptMediaBufferCopyLease(t, blocker)}
	request := buffer.register()
	ctx, cancel := context.WithCancel(context.Background())
	writer := newMediaBufferBlockingWriter(nil)
	waiting := make(chan struct{})
	var waitOnce sync.Once
	hooks := &mediaBufferCopyHooks{onOptionalWait: func() { waitOnce.Do(func() { close(waiting) }) }}
	source := newMediaBufferTestSource(bytes.NewReader(bytes.Repeat([]byte("a"), 2*mediaCopyBufferSize)), nil)
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBodyWithHooks(ctx, writer, source, source, make([]byte, mediaCopyBufferSize), request, -1, hooks)
	}()
	awaitMediaBufferSignal(t, writer.started)
	awaitMediaBufferSignal(t, waiting)
	cancel()
	close(writer.release)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) || result.Direction != "" {
		t.Fatalf("result=%+v", result)
	}
	if got := request.snapshot(); got.Owned != 0 || got.Requesting || got.Pending {
		t.Fatalf("request after cancellation=%+v", got)
	}
	for _, lease := range blockerLeases {
		if err := blocker.releaseOptional(lease); err != nil {
			t.Fatal(err)
		}
	}
	closeMediaBufferCopyRequests(t, blocker, request)
}

func TestMediaBufferCopyPendingGrantCanceledBeforeAcceptancePreservesDownstreamResult(t *testing.T) {
	writeErr := errors.New("downstream write failed")
	for _, tt := range []struct {
		name      string
		blocking  bool
		writerN   int
		writerErr error
	}{
		{name: "immediate notify write error", writerN: 0, writerErr: writeErr},
		{name: "blocking notify short write", blocking: true, writerN: mediaCopyBufferSize - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			budget := mediaBufferChunkSize
			if tt.blocking {
				budget = 2 * mediaBufferChunkSize
			}
			controller := mustMediaBufferCopyController(t, budget)
			var blocker *mediaBufferRequest
			var blockerLeases []mediaBufferLease
			if tt.blocking {
				blocker = controller.register()
				blockerLeases = []mediaBufferLease{acceptMediaBufferCopyLease(t, blocker), acceptMediaBufferCopyLease(t, blocker)}
			}
			request := controller.register()
			notified := make(chan struct{})
			allowAccept := make(chan struct{})
			cleanupCanceled := make(chan struct{})
			optionalWait := make(chan struct{})
			var notifiedOnce, cleanupOnce, waitOnce sync.Once
			hooks := &mediaBufferCopyHooks{
				onOptionalWait: func() { waitOnce.Do(func() { close(optionalWait) }) },
				onOptionalNotified: func() {
					notifiedOnce.Do(func() { close(notified) })
					<-allowAccept
				},
				onCleanupCanceled: func() { cleanupOnce.Do(func() { close(cleanupCanceled) }) },
			}
			writer := &mediaBufferNotifiedFailureWriter{notified: notified, n: tt.writerN, err: tt.writerErr}
			source := newMediaBufferTestSource(bytes.NewReader(bytes.Repeat([]byte("p"), 2*mediaCopyBufferSize)), nil)
			resultCh := make(chan mediaCopyResult, 1)
			go func() {
				resultCh <- copyBufferedMediaBodyWithHooks(context.Background(), writer, source, source, make([]byte, mediaCopyBufferSize), request, -1, hooks)
			}()
			if tt.blocking {
				awaitMediaBufferSignal(t, optionalWait)
				if err := blocker.releaseOptional(blockerLeases[0]); err != nil {
					t.Fatal(err)
				}
			}
			awaitMediaBufferSignal(t, notified)
			awaitMediaBufferSignal(t, cleanupCanceled)
			if got := request.snapshot(); got.Owned != 0 || got.Pending || got.Requesting {
				t.Fatalf("request after pending reclaim=%+v", got)
			}
			requireNoMediaBufferResult(t, resultCh)
			close(allowAccept)
			result := awaitMediaBufferResult(t, resultCh)
			syncResult := copyMediaBody(&mediaBufferInvalidWriter{n: tt.writerN, err: tt.writerErr}, bytes.NewReader(bytes.Repeat([]byte("p"), mediaCopyBufferSize)), -1)
			assertMediaCopyParity(t, result, syncResult)
			if errors.Is(result.Err, errMediaBufferCopyInvariant) {
				t.Fatalf("expected cancellation race produced invariant: %v", result.Err)
			}
			if tt.blocking {
				if err := blocker.releaseOptional(blockerLeases[1]); err != nil {
					t.Fatal(err)
				}
				closeMediaBufferCopyRequests(t, blocker)
			}
			closeMediaBufferCopyRequests(t, request)
		})
	}
}

func TestMediaBufferCopyCancellationUnblocksReadAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &mediaBufferJoinReader{started: make(chan struct{}), closed: make(chan struct{}), closeObserved: make(chan struct{}), allowReturn: make(chan struct{})}
	source := newMediaBufferTestSource(reader, func() { close(reader.closed) })
	buffer := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	request := buffer.register()
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBody(ctx, io.Discard, source, source, make([]byte, mediaCopyBufferSize), request, -1)
	}()
	awaitMediaBufferSignal(t, reader.started)
	cancel()
	awaitMediaBufferSignal(t, reader.closeObserved)
	requireNoMediaBufferResult(t, resultCh)
	close(reader.allowReturn)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) || source.closeCount() != 1 {
		t.Fatalf("result=%+v closes=%d", result, source.closeCount())
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyCancellationJoinsBlockedOptionalReadAndReleasesOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &mediaBufferOptionalBlockingReader{
		optionalRead:  make(chan struct{}),
		closed:        make(chan struct{}),
		closeObserved: make(chan struct{}),
		allowReturn:   make(chan struct{}),
	}
	source := newMediaBufferTestSource(reader, func() { close(reader.closed) })
	writer := newMediaBufferBlockingWriter(nil)
	buffer := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	request := buffer.register()
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBody(ctx, writer, source, source, make([]byte, mediaCopyBufferSize), request, -1)
	}()
	awaitMediaBufferSignal(t, writer.started)
	awaitMediaBufferSignal(t, reader.optionalRead)
	cancel()
	close(writer.release)
	awaitMediaBufferSignal(t, reader.closeObserved)
	requireNoMediaBufferResult(t, resultCh)
	close(reader.allowReturn)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("result=%+v", result)
	}
	if got := request.snapshot(); got.Owned != 0 || got.Pending || got.Requesting {
		t.Fatalf("request before close=%+v", got)
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyBlockedWriteResultWinsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	downstreamErr := errors.New("blocked write failed")
	writer := newMediaBufferBlockingWriter(downstreamErr)
	resultCh, source, request := startMediaBufferCopy(t, ctx, writer, &mediaBufferNErrorReader{data: []byte("media"), err: io.EOF}, 5, mediaBufferChunkSize, nil)
	awaitMediaBufferSignal(t, writer.started)
	cancel()
	close(writer.release)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, downstreamErr) || result.Direction != mediaDirectionDownstream {
		t.Fatalf("result=%+v", result)
	}
	assertMediaBufferSourceClosedOnce(t, source)
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyCancellationDropsQueuedData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	injected := errors.New("injected queued release failure")
	writer := newMediaBufferBlockingWriter(nil)
	reader := &mediaBufferReadCountReader{data: bytes.Repeat([]byte("q"), 3*mediaCopyBufferSize), reached: make(chan struct{})}
	hooks := &mediaBufferCopyHooks{injectReleaseErr: injected}
	resultCh, _, request := startMediaBufferCopy(t, ctx, writer, reader, -1, 2*mediaBufferChunkSize, hooks)
	awaitMediaBufferSignal(t, writer.started)
	awaitMediaBufferSignal(t, reader.reached)
	cancel()
	close(writer.release)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) || !errors.Is(result.Err, errMediaBufferCopyInvariant) || !errors.Is(result.Err, injected) || writer.calls.Load() != 1 || result.BytesWritten != mediaCopyBufferSize {
		t.Fatalf("result=%+v writes=%d", result, writer.calls.Load())
	}
	if got := request.snapshot(); got.Owned != 0 || got.Requesting || got.Pending {
		t.Fatalf("request after queued cleanup=%+v", got)
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyNoPublicationAfterClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &mediaBufferReturnAfterCloseReader{started: make(chan struct{}), closed: make(chan struct{})}
	source := newMediaBufferTestSource(reader, func() { close(reader.closed) })
	writer := &mediaBufferCountingWriter{}
	buffer := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	request := buffer.register()
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBody(ctx, writer, source, source, make([]byte, mediaCopyBufferSize), request, -1)
	}()
	awaitMediaBufferSignal(t, reader.started)
	cancel()
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) || writer.calls.Load() != 0 || result.BytesRead != 5 || result.BytesWritten != 0 {
		t.Fatalf("result=%+v writes=%d", result, writer.calls.Load())
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCopyBurstPauseReadsAheadOfBlockedWriter(t *testing.T) {
	bufferedPause := make(chan struct{})
	bufferedReader := newMediaBufferStagedReader(bufferedPause)
	bufferedWriter := newMediaBufferBlockingWriter(nil)
	bufferedResultCh, _, bufferedRequest := startMediaBufferCopy(t, context.Background(), bufferedWriter, bufferedReader, -1, 2*mediaBufferChunkSize, nil)
	awaitMediaBufferSignal(t, bufferedWriter.started)
	awaitMediaBufferSignal(t, bufferedReader.secondRead)
	close(bufferedWriter.release)
	close(bufferedPause)
	bufferedResult := awaitMediaBufferResult(t, bufferedResultCh)
	if bufferedResult.Err != nil {
		t.Fatal(bufferedResult.Err)
	}
	closeMediaBufferCopyRequests(t, bufferedRequest)

	syncPause := make(chan struct{})
	syncReader := newMediaBufferStagedReader(syncPause)
	syncWriter := newMediaBufferBlockingWriter(nil)
	syncResultCh := make(chan mediaCopyResult, 1)
	go func() { syncResultCh <- copyMediaBody(syncWriter, syncReader, -1) }()
	awaitMediaBufferSignal(t, syncWriter.started)
	requireNoMediaBufferSignal(t, syncReader.secondRead)
	close(syncWriter.release)
	awaitMediaBufferSignal(t, syncReader.secondRead)
	close(syncPause)
	syncResult := awaitMediaBufferResult(t, syncResultCh)
	if syncResult.Err != nil || bufferedResult.BytesWritten != syncResult.BytesWritten {
		t.Fatalf("buffered=%+v sync=%+v", bufferedResult, syncResult)
	}
}

func runMediaBufferCopy(t *testing.T, ctx context.Context, dst io.Writer, reader io.Reader, expectedLength, budget int64, hooks *mediaBufferCopyHooks) (mediaCopyResult, *mediaBufferTestSource) {
	t.Helper()
	buffer := mustMediaBufferCopyController(t, budget)
	request := buffer.register()
	source := newMediaBufferTestSource(reader, nil)
	result := copyBufferedMediaBodyWithHooks(ctx, dst, source, source, make([]byte, mediaCopyBufferSize), request, expectedLength, hooks)
	if got := request.snapshot(); got.Owned != 0 || got.Requesting || got.Pending {
		t.Fatalf("request after copy=%+v", got)
	}
	closeMediaBufferCopyRequests(t, request)
	return result, source
}

func startMediaBufferCopy(t *testing.T, ctx context.Context, dst io.Writer, reader io.Reader, expectedLength, budget int64, hooks *mediaBufferCopyHooks) (<-chan mediaCopyResult, *mediaBufferTestSource, *mediaBufferRequest) {
	t.Helper()
	buffer := mustMediaBufferCopyController(t, budget)
	request := buffer.register()
	source := newMediaBufferTestSource(reader, nil)
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBodyWithHooks(ctx, dst, source, source, make([]byte, mediaCopyBufferSize), request, expectedLength, hooks)
	}()
	return resultCh, source, request
}

func assertMediaCopyParity(t *testing.T, got, want mediaCopyResult) {
	t.Helper()
	if got.BytesRead != want.BytesRead || got.BytesWritten != want.BytesWritten || got.Direction != want.Direction || !errors.Is(got.Err, want.Err) || !errors.Is(want.Err, got.Err) {
		t.Fatalf("buffered=%+v synchronous=%+v", got, want)
	}
}

type mediaBufferTestSource struct {
	reader  io.Reader
	onClose func()
	once    sync.Once
	closes  atomic.Int32
}

func newMediaBufferTestSource(reader io.Reader, onClose func()) *mediaBufferTestSource {
	return &mediaBufferTestSource{reader: reader, onClose: onClose}
}

func (s *mediaBufferTestSource) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *mediaBufferTestSource) Close() error {
	s.once.Do(func() {
		s.closes.Add(1)
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}
func (s *mediaBufferTestSource) closeCount() int32 { return s.closes.Load() }

type mediaBufferNErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *mediaBufferNErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

type mediaBufferEmptyReader struct{ reads int }

func (r *mediaBufferEmptyReader) Read([]byte) (int, error) {
	r.reads++
	return 0, nil
}

type mediaBufferResetEmptyReader struct{ reads int }

func (r *mediaBufferResetEmptyReader) Read(p []byte) (int, error) {
	switch {
	case r.reads < 99:
		r.reads++
		return 0, nil
	case r.reads == 99:
		r.reads++
		return copy(p, []byte("x")), nil
	case r.reads < 199:
		r.reads++
		return 0, nil
	default:
		return 0, io.EOF
	}
}

type mediaBufferInvalidReader struct{ n int }

func (r mediaBufferInvalidReader) Read([]byte) (int, error) { return r.n, nil }

type mediaBufferInvalidWriter struct {
	n   int
	err error
}

func (w *mediaBufferInvalidWriter) Write([]byte) (int, error) { return w.n, w.err }

type mediaBufferNotifiedFailureWriter struct {
	notified <-chan struct{}
	n        int
	err      error
}

func (w *mediaBufferNotifiedFailureWriter) Write([]byte) (int, error) {
	<-w.notified
	return w.n, w.err
}

type mediaBufferTerminalBarrierWriter struct{ terminalPublished <-chan struct{} }

func (w *mediaBufferTerminalBarrierWriter) Write(p []byte) (int, error) {
	<-w.terminalPublished
	return len(p), nil
}

type mediaBufferBlockingWriter struct {
	started chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
	calls   atomic.Int32
}

func newMediaBufferBlockingWriter(err error) *mediaBufferBlockingWriter {
	return &mediaBufferBlockingWriter{started: make(chan struct{}), release: make(chan struct{}), err: err}
}

func (w *mediaBufferBlockingWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	w.once.Do(func() { close(w.started) })
	<-w.release
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

type mediaBufferFailSecondWriter struct {
	started      chan struct{}
	releaseFirst chan struct{}
	err          error
	calls        int
}

func newMediaBufferFailSecondWriter(err error) *mediaBufferFailSecondWriter {
	return &mediaBufferFailSecondWriter{started: make(chan struct{}), releaseFirst: make(chan struct{}), err: err}
}

func (w *mediaBufferFailSecondWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		close(w.started)
		<-w.releaseFirst
		return len(p), nil
	}
	return 0, w.err
}

type mediaBufferCountingWriter struct{ calls atomic.Int32 }

func (w *mediaBufferCountingWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	return len(p), nil
}

type mediaBufferJoinReader struct {
	started       chan struct{}
	closed        chan struct{}
	closeObserved chan struct{}
	allowReturn   chan struct{}
	once          sync.Once
}

func (r *mediaBufferJoinReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	close(r.closeObserved)
	<-r.allowReturn
	return 0, errors.New("closed")
}

type mediaBufferInvalidOptionalReader struct {
	step         int
	optionalRead chan struct{}
}

func (r *mediaBufferInvalidOptionalReader) Read(p []byte) (int, error) {
	if r.step == 0 {
		r.step++
		return copy(p, bytes.Repeat([]byte("a"), mediaCopyBufferSize)), nil
	}
	close(r.optionalRead)
	return len(p) + 1, nil
}

type mediaBufferOptionalBlockingReader struct {
	step          int
	optionalRead  chan struct{}
	closed        chan struct{}
	closeObserved chan struct{}
	allowReturn   chan struct{}
}

func (r *mediaBufferOptionalBlockingReader) Read(p []byte) (int, error) {
	if r.step == 0 {
		r.step++
		return copy(p, bytes.Repeat([]byte("a"), mediaCopyBufferSize)), nil
	}
	close(r.optionalRead)
	<-r.closed
	close(r.closeObserved)
	<-r.allowReturn
	return 0, errors.New("closed optional read")
}

type mediaBufferReturnAfterCloseReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *mediaBufferReturnAfterCloseReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return copy(p, []byte("media")), io.EOF
}

type mediaBufferReadCountReader struct {
	data    []byte
	off     int
	reads   int
	reached chan struct{}
	once    sync.Once
}

func (r *mediaBufferReadCountReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	r.reads++
	if r.reads == 2 {
		r.once.Do(func() { close(r.reached) })
	}
	return n, nil
}

type mediaBufferStagedReader struct {
	pause      chan struct{}
	secondRead chan struct{}
	step       int
	once       sync.Once
}

func newMediaBufferStagedReader(pause chan struct{}) *mediaBufferStagedReader {
	return &mediaBufferStagedReader{pause: pause, secondRead: make(chan struct{})}
}

func (r *mediaBufferStagedReader) Read(p []byte) (int, error) {
	switch r.step {
	case 0:
		r.step++
		return copy(p, bytes.Repeat([]byte("a"), mediaCopyBufferSize)), nil
	case 1:
		r.step++
		r.once.Do(func() { close(r.secondRead) })
		return copy(p, bytes.Repeat([]byte("b"), mediaCopyBufferSize)), nil
	case 2:
		r.step++
		<-r.pause
		return copy(p, bytes.Repeat([]byte("c"), mediaCopyBufferSize)), nil
	default:
		return 0, io.EOF
	}
}

func mustMediaBufferCopyController(t *testing.T, budget int64) *mediaBuffer {
	t.Helper()
	buffer, err := newMediaBuffer(budget)
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

func acceptMediaBufferCopyLease(t *testing.T, request *mediaBufferRequest) mediaBufferLease {
	t.Helper()
	notify, err := request.requestOptional()
	if err != nil {
		t.Fatal(err)
	}
	awaitMediaBufferSignal(t, notify)
	lease, err := request.acceptOptional()
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func closeMediaBufferCopyRequests(t *testing.T, requests ...*mediaBufferRequest) {
	t.Helper()
	for _, request := range requests {
		if err := request.close(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMediaBufferSourceClosedOnce(t *testing.T, source *mediaBufferTestSource) {
	t.Helper()
	if source.closeCount() != 1 {
		t.Fatalf("source close count=%d", source.closeCount())
	}
	_ = source.Close()
	if source.closeCount() != 1 {
		t.Fatalf("shared close count after outer close=%d", source.closeCount())
	}
}

func awaitMediaBufferSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for deterministic test event")
	}
}

func requireNoMediaBufferSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
		t.Fatal("unexpected test event")
	default:
	}
}

func awaitMediaBufferResult(t *testing.T, result <-chan mediaCopyResult) mediaCopyResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for media buffer copy")
		return mediaCopyResult{}
	}
}

func requireNoMediaBufferResult(t *testing.T, result <-chan mediaCopyResult) {
	t.Helper()
	select {
	case value := <-result:
		t.Fatalf("copy returned before producer join: %+v", value)
	default:
	}
}
