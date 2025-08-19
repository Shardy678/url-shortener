package analytics

import "time"

type Event struct {
	Timestamp time.Time `json:"timestamp"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Device    string    `json:"device"`
	OS        string    `json:"os"`
	Browser   string    `json:"browser"`
}

type EventWithCode struct {
	Code string
	E    Event
}
