package gateway

// ModbusListenSource: the passive RS485 tap as a gateway source. A
// background reader feeds the decoder; Poll drains whatever the bus
// revealed since the last flush, each reading stamped with its own
// observation time. Auto-baud: with Baud 0 the source captures a few
// seconds at each common rate and keeps the one that decodes frames.

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

type ModbusListenSource struct {
	Zone      string
	Every     time.Duration
	Registers []ModbusRegisterMap
	Device    string
	Baud      int // 0 = auto-detect
	Parity    string
	// Open abstracts the serial layer for tests.
	Open func(device string, baud int, parity string) (io.ReadCloser, error)

	mu       sync.Mutex
	pending  []Reading
	unmapped map[string]int
	stats    map[string]*RegisterStat
	started  bool
	decoder  *Decoder
}

// RegisterStat summarizes every observation of one bus register - the raw
// material of the on-site mapping session.
type RegisterStat struct {
	Count          int
	Last, Min, Max uint16
	Written        int
}

func (s *ModbusListenSource) Describe() string        { return "modbus-listen" }
func (s *ModbusListenSource) Simulated() bool         { return false }
func (s *ModbusListenSource) Interval() time.Duration { return s.Every }

// Poll starts the reader lazily and drains the observed readings.
func (s *ModbusListenSource) Poll(now time.Time) ([]Reading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.started = true
		s.unmapped = map[string]int{}
		s.stats = map[string]*RegisterStat{}
		s.decoder = NewDecoder()
		go s.readLoop()
	}
	out := s.pending
	s.pending = nil
	return out, nil
}

// Unmapped reports registers seen on the bus that no mapping names - the
// bus never hides anything.
func (s *ModbusListenSource) Unmapped() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.unmapped))
	for k, v := range s.unmapped {
		out[k] = v
	}
	return out
}

func statKey(o Observation) string {
	return fmt.Sprintf("%s/%d/%d", o.Table, o.Slave, o.Address)
}

// Observed snapshots the per-register statistics for the inspect mode.
func (s *ModbusListenSource) Observed() map[string]RegisterStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RegisterStat, len(s.stats))
	for k, v := range s.stats {
		out[k] = *v
	}
	return out
}

func (s *ModbusListenSource) readLoop() {
	baud := s.Baud
	if baud == 0 {
		baud = s.autoBaud()
		if baud == 0 {
			log.Printf("modbus-listen: no valid frames at any common baud; is the tap wired and the bus active?")
			s.mu.Lock()
			s.started = false // allow a later retry
			s.mu.Unlock()
			return
		}
		log.Printf("modbus-listen: locked baud %d", baud)
	}
	for {
		port, err := s.Open(s.Device, baud, s.Parity)
		if err != nil {
			log.Printf("modbus-listen open: %v (retrying in 10s)", err)
			time.Sleep(10 * time.Second)
			continue
		}
		buf := make([]byte, 512)
		for {
			n, err := port.Read(buf)
			if err != nil {
				log.Printf("modbus-listen read: %v (reopening)", err)
				break
			}
			if n == 0 {
				continue // VTIME poll timeout: bus quiet
			}
			s.mu.Lock()
			obs := s.decoder.Feed(buf[:n])
			readings, unmapped := ObservationsToReadings(obs, s.Registers, s.Zone)
			s.pending = append(s.pending, readings...)
			for k, v := range unmapped {
				s.unmapped[k] += v
			}
			for _, o := range obs {
				key := statKey(o)
				st, ok := s.stats[key]
				if !ok {
					st = &RegisterStat{Min: o.Raw, Max: o.Raw}
					s.stats[key] = st
				}
				st.Count++
				st.Last = o.Raw
				if o.Raw < st.Min {
					st.Min = o.Raw
				}
				if o.Raw > st.Max {
					st.Max = o.Raw
				}
				if o.Written {
					st.Written++
				}
			}
			s.mu.Unlock()
		}
		port.Close()
	}
}

// autoBaud captures a few seconds at each common rate and scores the
// decoded frames. Read-only, like everything here.
func (s *ModbusListenSource) autoBaud() int {
	best, bestScore := 0, 0
	for _, baud := range CommonBauds {
		port, err := s.Open(s.Device, baud, s.Parity)
		if err != nil {
			continue
		}
		capture := make([]byte, 0, 4096)
		buf := make([]byte, 512)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && len(capture) < 4096 {
			n, err := port.Read(buf)
			if err != nil {
				break
			}
			capture = append(capture, buf[:n]...)
		}
		port.Close()
		score := ScoreStream(capture)
		log.Printf("modbus-listen: baud %d -> %d frames in %d bytes", baud, score, len(capture))
		if score > bestScore {
			best, bestScore = baud, score
		}
	}
	if bestScore < 3 {
		return 0 // fewer than 3 clean frames is noise, not a lock
	}
	return best
}
