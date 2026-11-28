// Package apitime gives the API's times in UTC, whatever zone the hub runs in.
package apitime

import "time"

func UTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
