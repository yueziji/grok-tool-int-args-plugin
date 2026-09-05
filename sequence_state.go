package main

import "sync"

// The host can evict its entire history after one oversized chunk. Retain the
// next sequence until request.complete so even long-running streams can resume.
var responseSequences = struct {
	sync.Mutex
	next map[string]int
}{next: make(map[string]int)}

func responseSequenceFallback(requestID string) int {
	responseSequences.Lock()
	defer responseSequences.Unlock()
	return responseSequences.next[requestID]
}

func storeResponseSequence(requestID string, next int) {
	if requestID == "" {
		return
	}
	responseSequences.Lock()
	responseSequences.next[requestID] = next
	responseSequences.Unlock()
}

func clearResponseSequence(requestID string) {
	responseSequences.Lock()
	delete(responseSequences.next, requestID)
	responseSequences.Unlock()
}

func resetResponseSequences() {
	responseSequences.Lock()
	clear(responseSequences.next)
	responseSequences.Unlock()
}
