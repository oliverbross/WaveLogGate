package radio

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"waveloggate/internal/config"
	"waveloggate/internal/debug"
	"waveloggate/internal/wavelog"
)

const observationReportInterval = 5 * time.Second

// Poller polls a RadioClient every second and reports changes.
type Poller struct {
	mu                sync.Mutex
	client            RadioClient
	cfg               *config.Profile
	wlClient          *wavelog.Client
	onPoll            PollCallback
	lastStatus        RigStatus
	lastReport        time.Time
	lastFailureReport time.Time
	failed            bool
	sessionID         string
	sequence          uint64
	reportInterval    time.Duration
	now               func() time.Time
	cancel            context.CancelFunc
}

// NewPoller creates a Poller. Successful unchanged reads are reported at a
// bounded cadence so consumers can distinguish observation freshness from
// cached transport state.
func NewPoller(cfg *config.Profile, wlClient *wavelog.Client, onPoll PollCallback) *Poller {
	return &Poller{
		cfg:            cfg,
		wlClient:       wlClient,
		onPoll:         onPoll,
		sessionID:      newSessionID(),
		reportInterval: observationReportInterval,
		now:            time.Now,
	}
}

// UpdateConfig updates the profile and radio client.
func (p *Poller) UpdateConfig(cfg *config.Profile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
	p.client = buildClient(cfg)
}

// Start begins polling in a background goroutine.
func (p *Poller) Start(ctx context.Context) {
	p.mu.Lock()
	p.client = buildClient(p.cfg)
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			p.poll()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop stops the poller.
func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// SetFreqMode issues a QSY command through the current radio client.
func (p *Poller) SetFreqMode(hz int64, mode string) error {
	p.mu.Lock()
	client := p.client
	cfg := p.cfg
	p.mu.Unlock()

	if client == nil {
		return fmt.Errorf("no radio client configured")
	}

	// Reverse satellite offset: the incoming frequency is the displayed
	// (offset-corrected) value. Subtract the offset so the radio receives
	// the IF frequency.
	if cfg.SatEnabled && cfg.SatRxOffsetMHz != 0 {
		hz = ReverseSatOffset(hz, cfg.SatRxOffsetMHz)
		debug.Log("[QSY] sat reverse RX offset → %d Hz", hz)
	}

	modes, _ := client.GetModes()
	debug.Log("[QSY] requested mode=%q freq=%d available=%v", mode, hz, modes)

	targetMode := SelectMode(mode, hz, modes)
	debug.Log("[QSY] resolved targetMode=%q WavelogPmode=%v", targetMode, cfg.WavelogPmode)

	setMode := ""
	if cfg.WavelogPmode && targetMode != "" {
		setMode = targetMode
	}

	debug.Log("[QSY] sending to radio: freq=%d mode=%q", hz, setMode)
	return client.SetFreqMode(hz, setMode)
}

// SetTxFreq sets the TX (VFO B) frequency in split mode.
func (p *Poller) SetTxFreq(hz int64) error {
	p.mu.Lock()
	client := p.client
	cfg := p.cfg
	p.mu.Unlock()

	if client == nil {
		return fmt.Errorf("no radio client configured")
	}

	// Reverse satellite offset for TX frequency.
	if cfg.SatEnabled && cfg.SatTxOffsetMHz != 0 {
		hz = ReverseSatOffset(hz, cfg.SatTxOffsetMHz)
		debug.Log("[QSY] sat reverse TX offset → %d Hz", hz)
	}

	return client.SetTxFreq(hz)
}

func (p *Poller) poll() {
	p.mu.Lock()
	client := p.client
	cfg := p.cfg
	sessionID := p.sessionID
	p.mu.Unlock()

	if client == nil {
		return
	}

	p.mu.Lock()
	p.sequence++
	sequence := p.sequence
	p.mu.Unlock()

	status, err := client.GetStatus()
	if err != nil {
		now := p.now()
		p.mu.Lock()
		report := !p.failed || p.lastFailureReport.IsZero() || now.Sub(p.lastFailureReport) >= p.reportInterval
		p.failed = true
		if report {
			p.lastFailureReport = now
		}
		p.mu.Unlock()
		if report && p.onPoll != nil {
			p.onPoll(PollEvent{State: PollFailed, SessionID: sessionID, Sequence: sequence, EventAt: now, FailureCode: "RADIO_POLL_FAILED"})
		}
		return
	}
	now := p.now()

	// Optionally zero out power. HamlibClient already skips the RFPOWER read
	// when this is set; this is the backstop that also covers FLRig, which
	// reports power unconditionally.
	if cfg != nil && cfg.IgnorePwr {
		status.Power = 0
	}

	// Check for unsupported float power values
	if math.IsNaN(status.Power) || math.IsInf(status.Power, 0) {
		status.Power = 0
	}

	// Apply satellite/transverter frequency offsets.
	if cfg != nil {
		ApplySatOffsets(&status, cfg)
	}

	p.mu.Lock()
	changed := !statusEqual(status, p.lastStatus)
	recovered := p.failed
	reportDue := p.lastReport.IsZero() || now.Sub(p.lastReport) >= p.reportInterval
	p.failed = false
	if changed || recovered || reportDue {
		p.lastStatus = status
		p.lastReport = now
		p.mu.Unlock()

		event := PollEvent{State: PollObserved, Status: status, SessionID: sessionID, Sequence: sequence, ObservedAt: now, EventAt: now}
		if p.onPoll != nil {
			p.onPoll(event)
		}

		// Send to Wavelog.
		if p.wlClient != nil {
			data := wavelog.RadioData{
				Frequency:        int64(math.Round(status.FreqA)),
				Mode:             status.Mode,
				Power:            status.Power,
				Split:            status.Split,
				SourceSessionID:  sessionID,
				SourceSequence:   sequence,
				SourceObservedAt: now,
			}
			if status.Split {
				data.FrequencyRx = int64(math.Round(status.FreqB))
				data.ModeRx = status.ModeB
			}
			if cfg != nil && cfg.SatEnabled {
				data.PropMode = "SAT"
				data.SatName = cfg.SatName
				data.SatMode = cfg.SatMode
			}
			if err := p.wlClient.UpdateRadioStatus(data); err != nil {
				if p.onPoll != nil {
					p.onPoll(PollEvent{State: ServerDeliveryFailed, Status: status, SessionID: sessionID, Sequence: sequence, ObservedAt: now, EventAt: p.now(), FailureCode: "SERVER_DELIVERY_FAILED"})
				}
			} else if p.onPoll != nil {
				p.onPoll(PollEvent{State: ServerDelivered, Status: status, SessionID: sessionID, Sequence: sequence, ObservedAt: now, EventAt: p.now()})
			}
		}
	} else {
		p.mu.Unlock()
	}
}

func statusEqual(a, b RigStatus) bool {
	return a.FreqA == b.FreqA &&
		a.FreqB == b.FreqB &&
		a.Mode == b.Mode &&
		a.ModeB == b.ModeB &&
		a.Power == b.Power &&
		a.Split == b.Split &&
		a.PTT == b.PTT
}

func buildClient(cfg *config.Profile) RadioClient {
	if cfg == nil {
		return nil
	}
	switch {
	case cfg.FlrigEna:
		return NewFLRig(cfg.FlrigHost, cfg.FlrigPort)
	case cfg.HamlibEna:
		// Connect to rigctld via TCP for both internal (managed) and external modes
		// Internal mode: hamlib manager starts rigctld, poller connects to it
		// External mode: user runs rigctld manually, poller connects to it
		return NewHamlib(cfg.HamlibHost, cfg.HamlibPort, !cfg.IgnorePwr, cfg.HamlibMaxPower)
	default:
		return nil
	}
}
