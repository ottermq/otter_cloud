package httpapi

import "time"

// SetClock makes the server's rate limits use now, for the external tests.
func (o *Options) SetClock(now func() time.Time) { o.now = now }
