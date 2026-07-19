package gateway

func (b *mediaBuffer) assertInvariants() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assertInvariantsLocked()
}

func (b *mediaBuffer) assertInvariantsLocked() {
	b.assertAccountingLocked()
	seenChunks := make(map[*mediaBufferChunk]string, b.allocated/mediaBufferChunkSize)
	seenGenerations := make(map[uint64]struct{}, b.owned/mediaBufferChunkSize)
	for _, chunk := range b.free {
		if chunk == nil || len(chunk.data) != int(mediaBufferChunkSize) || chunk.ownerID != 0 {
			panic("invalid free media buffer chunk")
		}
		if _, exists := seenChunks[chunk]; exists {
			panic("duplicate free media buffer chunk")
		}
		seenChunks[chunk] = "free"
	}

	requestSet := make(map[*mediaBufferRequest]struct{}, len(b.requests))
	requestIDs := make(map[uint64]struct{}, len(b.requests))
	expectedTarget := int64(0)
	if len(b.requests) > 0 {
		expectedTarget = alignMediaBufferSize(b.hardBudget / int64(len(b.requests)))
		if expectedTarget > mediaBufferRequestCap {
			expectedTarget = mediaBufferRequestCap
		}
	}
	var requestOwned int64
	for _, request := range b.requests {
		if request == nil || request.buffer != b || request.closed {
			panic("invalid registered media buffer request")
		}
		if _, exists := requestSet[request]; exists {
			panic("duplicate registered media buffer request")
		}
		if _, exists := requestIDs[request.id]; exists || request.id == 0 {
			panic("duplicate media buffer request id")
		}
		requestSet[request] = struct{}{}
		requestIDs[request.id] = struct{}{}
		expectedDebt := request.owned - request.target
		if expectedDebt < 0 {
			expectedDebt = 0
		}
		if request.target != expectedTarget || request.debt != expectedDebt || request.owned < 0 || request.owned%mediaBufferChunkSize != 0 {
			panic("invalid media buffer request accounting")
		}
		expectedOwned := int64(len(request.chunks)) * mediaBufferChunkSize
		if request.pending != nil {
			expectedOwned += mediaBufferChunkSize
		}
		if request.owned != expectedOwned {
			panic("media buffer request ownership map mismatch")
		}
		for chunk, generation := range request.chunks {
			if chunk == nil || generation == 0 || chunk.ownerID != request.id || chunk.generation != generation || len(chunk.data) != int(mediaBufferChunkSize) {
				panic("invalid accepted media buffer lease")
			}
			if _, exists := seenChunks[chunk]; exists {
				panic("media buffer chunk has multiple owners")
			}
			if _, exists := seenGenerations[generation]; exists {
				panic("duplicate active media buffer generation")
			}
			seenChunks[chunk] = "accepted"
			seenGenerations[generation] = struct{}{}
		}
		if request.pending != nil {
			lease := request.pending
			if lease.chunk == nil || lease.requestID != request.id || lease.generation == 0 || lease.chunk.ownerID != request.id || lease.chunk.generation != lease.generation || request.waiting {
				panic("invalid pending media buffer lease")
			}
			if _, exists := seenChunks[lease.chunk]; exists {
				panic("pending media buffer chunk has multiple owners")
			}
			if _, exists := seenGenerations[lease.generation]; exists {
				panic("duplicate active media buffer generation")
			}
			seenChunks[lease.chunk] = "pending"
			seenGenerations[lease.generation] = struct{}{}
		} else if len(request.notify) != 0 {
			panic("media buffer notification without pending lease")
		}
		requestOwned += request.owned
	}
	if requestOwned != b.owned || int64(len(seenChunks))*mediaBufferChunkSize != b.allocated {
		panic("media buffer aggregate ownership mismatch")
	}

	waiterSet := make(map[*mediaBufferRequest]struct{}, len(b.waiters))
	for _, request := range b.waiters {
		if _, registered := requestSet[request]; !registered || request.closed || !request.waiting || request.pending != nil {
			panic("invalid media buffer waiter")
		}
		if _, exists := waiterSet[request]; exists {
			panic("duplicate media buffer waiter")
		}
		waiterSet[request] = struct{}{}
	}
	for _, request := range b.requests {
		_, queued := waiterSet[request]
		if queued != request.waiting {
			panic("media buffer waiter state mismatch")
		}
	}
}
