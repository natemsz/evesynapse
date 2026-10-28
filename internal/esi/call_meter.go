package esi

import (
	"time"
)

// Call metering: every HTTP attempt against ESI lands in an
// hourly bucket, so the Sync page can show calls per active pilot
// per day over the trailing 24 hours. In-process only; a restart
// resets the window.

// FamilyCount is one endpoint family's attempts and failures
// since the last TakeFamilyCounts.
type FamilyCount struct {
	Calls  int64
	Errors int64
}

// familyOf names the endpoint family a path belongs to: its first
// segment ("/markets/10000002/orders/" is "markets"). A path with
// no segment is "other".
func familyOf(path string) string {
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			if i == 1 {
				break
			}
			return path[1:i]
		}
	}
	if len(path) > 1 {
		return path[1:]
	}
	return "other"
}

// noteCallOutcome records one finished attempt under its family.
// It runs deferred in sendOnce, so every return path counts, with
// the error the caller is about to see.
func (c *Client) noteCallOutcome(path string, err error) {
	fam := familyOf(path)
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	if c.callsByFamily == nil {
		c.callsByFamily = map[string]*FamilyCount{}
	}
	fc := c.callsByFamily[fam]
	if fc == nil {
		fc = &FamilyCount{}
		c.callsByFamily[fam] = fc
	}
	fc.Calls++
	if err != nil {
		fc.Errors++
	}
}

// TakeFamilyCounts drains the per-family counts gathered since
// the last call. The Admin analytics recorder flushes them into
// its per-minute rows once a minute.
func (c *Client) TakeFamilyCounts() map[string]FamilyCount {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	out := make(map[string]FamilyCount, len(c.callsByFamily))
	for fam, fc := range c.callsByFamily {
		out[fam] = *fc
	}
	c.callsByFamily = nil
	return out
}

// noteCall records one HTTP call. It sits at the top of sendOnce,
// so retries count separately, like ESI counts them.
func (c *Client) noteCall() {
	c.noteCallAt(time.Now())
}

func (c *Client) noteCallAt(now time.Time) {
	hour := now.Unix() / 3600
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	if c.callsByHour == nil {
		c.callsByHour = map[int64]int64{}
	}
	c.callsByHour[hour]++
	c.callsTotal++
	for h := range c.callsByHour {
		if h < hour-23 {
			delete(c.callsByHour, h)
		}
	}
}

// CallsTotal is every HTTP call this client has made.
func (c *Client) CallsTotal() int64 {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	return c.callsTotal
}

// CallsLastDay is the calls in the trailing 24 hours.
func (c *Client) CallsLastDay() int64 {
	return c.callsLastDayAt(time.Now())
}

func (c *Client) callsLastDayAt(now time.Time) int64 {
	cutoff := now.Unix()/3600 - 23
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	var n int64
	for h, count := range c.callsByHour {
		if h >= cutoff {
			n += count
		}
	}
	return n
}
