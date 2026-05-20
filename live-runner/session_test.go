package main

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

func TestSessionStoreNextPort(t *testing.T) {
	s := newSessionStore()
	port, err := s.nextPort(1000, 1002)
	if err != nil || port != 1000 {
		t.Fatalf("port=%d err=%v", port, err)
	}
	rt := &sessionRuntime{rec: sessionRecord{RunnerSessionID: "a", RTMPPort: 1000}}
	if err := s.add(rt); err != nil {
		t.Fatal(err)
	}
	port, err = s.nextPort(1000, 1002)
	if err != nil || port != 1001 {
		t.Fatalf("port=%d err=%v", port, err)
	}
}

func TestMarkProgressTransitionsToPublishing(t *testing.T) {
	rt := &sessionRuntime{rec: sessionRecord{State: stateReady}}
	delta, started := rt.markProgress(5)
	if !started || delta != 5 {
		t.Fatalf("started=%v delta=%d", started, delta)
	}
	if rt.rec.State != statePublishing {
		t.Fatalf("state=%s", rt.rec.State)
	}
}

func TestWatchdogNoPublishTimeout(t *testing.T) {
	rt := &sessionRuntime{
		rec: sessionRecord{
			RunnerSessionID: "a",
			State:           stateReady,
			CreatedAt:       time.Now().Add(-2 * time.Minute),
		},
	}
	if err := rt.watchdog(10 * time.Second); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestLogTailKeepsRecentLines(t *testing.T) {
	tail := newLogTail(3)
	tail.add("a")
	tail.add("b")
	tail.add("c")
	tail.add("d")
	if got := tail.join(); got != "b | c | d" {
		t.Fatalf("tail=%q", got)
	}
}

func TestScanCRLFSplitsProgressLines(t *testing.T) {
	scanner := bufio.NewScanner(bytes.NewBufferString("frame=   70 fps= 47\rframe=   85 fps= 42\nfinal"))
	scanner.Split(scanCRLF)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if len(got) != 3 || got[0] != "frame=   70 fps= 47" || got[1] != "frame=   85 fps= 42" || got[2] != "final" {
		t.Fatalf("tokens=%q", got)
	}
}
