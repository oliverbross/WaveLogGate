package radio

import (
	"errors"
	"testing"
	"time"

	"waveloggate/internal/config"
)

type pollReply struct {
	status RigStatus
	err    error
}

type fixtureRadioClient struct {
	replies []pollReply
	index   int
}

func (f *fixtureRadioClient) GetStatus() (RigStatus, error) {
	reply := f.replies[f.index]
	if f.index < len(f.replies)-1 {
		f.index++
	}
	return reply.status, reply.err
}
func (*fixtureRadioClient) SetFreqMode(int64, string) error { return nil }
func (*fixtureRadioClient) SetTxFreq(int64) error           { return nil }
func (*fixtureRadioClient) GetModes() ([]string, error)     { return nil, nil }

func testPoller(now *time.Time, client RadioClient, events *[]PollEvent) *Poller {
	p := NewPoller(&config.Profile{}, nil, func(event PollEvent) { *events = append(*events, event) })
	p.client = client
	p.sessionID = "session-a"
	p.reportInterval = 5 * time.Second
	p.now = func() time.Time { return *now }
	return p
}

func TestUnchangedSuccessfulPollsReportAtBoundedCadence(t *testing.T) {
	now := time.Date(2026, 10, 3, 2, 50, 0, 0, time.UTC)
	status := RigStatus{FreqA: 14042300, Mode: "USB"}
	events := []PollEvent{}
	p := testPoller(&now, &fixtureRadioClient{replies: []pollReply{{status: status}}}, &events)

	p.poll()
	now = now.Add(time.Second)
	p.poll()
	now = now.Add(4 * time.Second)
	p.poll()

	if len(events) != 2 {
		t.Fatalf("expected first and five-second observations, got %d", len(events))
	}
	if events[0].State != PollObserved || events[0].Sequence != 1 {
		t.Fatalf("unexpected first observation: %+v", events[0])
	}
	if events[1].State != PollObserved || events[1].Sequence != 3 {
		t.Fatalf("unchanged observation did not retain poll-attempt ordering: %+v", events[1])
	}
	if !events[1].ObservedAt.Equal(now) || events[1].Status != status {
		t.Fatalf("unexpected refreshed observation: %+v", events[1])
	}
}

func TestFailureDoesNotAdvanceObservationAndSameStateRecoveryReports(t *testing.T) {
	now := time.Date(2026, 10, 3, 2, 50, 0, 0, time.UTC)
	status := RigStatus{FreqA: 14042300, Mode: "USB"}
	client := &fixtureRadioClient{replies: []pollReply{
		{status: status},
		{err: errors.New("controller unavailable")},
		{err: errors.New("controller unavailable")},
		{status: status},
	}}
	events := []PollEvent{}
	p := testPoller(&now, client, &events)

	for range 4 {
		p.poll()
		now = now.Add(time.Second)
	}

	if len(events) != 3 {
		t.Fatalf("expected observation, first failure and same-state recovery, got %#v", events)
	}
	if events[0].State != PollObserved || events[0].Sequence != 1 {
		t.Fatalf("unexpected initial event: %+v", events[0])
	}
	if events[1].State != PollFailed || events[1].Sequence != 2 || events[1].FailureCode != "RADIO_POLL_FAILED" {
		t.Fatalf("unexpected failure event: %+v", events[1])
	}
	if !events[1].ObservedAt.IsZero() || !events[1].EventAt.Equal(events[0].ObservedAt.Add(time.Second)) {
		t.Fatalf("poll failure must not claim a successful observation: %+v", events[1])
	}
	if events[2].State != PollObserved || events[2].Sequence != 4 || events[2].Status != status {
		t.Fatalf("unchanged recovery was not a new observation: %+v", events[2])
	}
}

func TestPollerRestartCreatesNewSession(t *testing.T) {
	first := NewPoller(nil, nil, nil)
	second := NewPoller(nil, nil, nil)
	if first.sessionID == "" || second.sessionID == "" || first.sessionID == second.sessionID {
		t.Fatalf("poller sessions must be nonempty and unique: %q %q", first.sessionID, second.sessionID)
	}
}
