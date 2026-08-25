package main

// /stats — counters you'll want when tuning the protocol against the real
// model. In-memory, reset on restart. Latency is a simple ring for p50/p95.

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	Turns, ToolCalls, Nudges, Repairs, RepairFailures, NewConvs, ReusedConvs, Errors, Shrinks atomic.Int64
	mu                                                                                        sync.Mutex
	lat                                                                                       []time.Duration
}

func (s *Stats) observe(d time.Duration) {
	s.mu.Lock()
	s.lat = append(s.lat, d)
	if len(s.lat) > 512 {
		s.lat = s.lat[len(s.lat)-512:]
	}
	s.mu.Unlock()
}

func (s *Stats) percentiles() (p50, p95 time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lat) == 0 {
		return 0, 0
	}
	c := append([]time.Duration{}, s.lat...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2], c[len(c)*95/100]
}

func (s *Stats) handler(w http.ResponseWriter, _ *http.Request) {
	p50, p95 := s.percentiles()
	writeJSON(w, 200, map[string]any{
		"turns": s.Turns.Load(), "tool_calls": s.ToolCalls.Load(),
		"nudges": s.Nudges.Load(), "arg_repairs": s.Repairs.Load(), "arg_repair_failures": s.RepairFailures.Load(),
		"conversations_new": s.NewConvs.Load(), "conversations_reused": s.ReusedConvs.Load(),
		"message_shrinks": s.Shrinks.Load(), "errors": s.Errors.Load(),
		"latency_p50_ms": p50.Milliseconds(), "latency_p95_ms": p95.Milliseconds(),
	})
}
