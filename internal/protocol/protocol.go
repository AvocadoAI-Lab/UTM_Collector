package protocol

import "encoding/json"

type Event struct {
	ReceivedAt string          `json:"received_at"`
	SourceIP   string          `json:"source_ip,omitempty"`
	Raw        string          `json:"raw"`
	RawBase64  string          `json:"raw_base64,omitempty"`
	Parsed     json.RawMessage `json:"parsed"`
	ParseError *string         `json:"parse_error"`
}

type EventsRequest struct {
	AgentID      string  `json:"agent_id"`
	SiteID       string  `json:"site_id"`
	AgentVersion string  `json:"agent_version"`
	BatchID      string  `json:"batch_id"`
	SentAt       string  `json:"sent_at"`
	Events       []Event `json:"events"`
}

type EventsResponse struct {
	OK         bool `json:"ok"`
	Received   int  `json:"received"`
	Accepted   int  `json:"accepted"`
	Duplicated int  `json:"duplicated"`
}

type HeartbeatRequest struct {
	AgentID                 string   `json:"agent_id"`
	SiteID                  string   `json:"site_id"`
	AgentVersion            string   `json:"agent_version"`
	Hostname                string   `json:"hostname"`
	OS                      string   `json:"os"`
	SentAt                  string   `json:"sent_at"`
	UptimeSeconds           int64    `json:"uptime_seconds"`
	LastEventReceivedAt     *string  `json:"last_event_received_at"`
	EventsReceivedTotal     int64    `json:"events_received_total"`
	EventsForwardedTotal    int64    `json:"events_forwarded_total"`
	EventsDeadLetteredTotal int64    `json:"events_dead_lettered_total"`
	EventsDroppedTotal      int64    `json:"events_dropped_total"`
	SpoolWriteErrorsTotal   int64    `json:"spool_write_errors_total"`
	SpoolBacklogBytes       int64    `json:"spool_backlog_bytes"`
	SpoolTotalBytes         int64    `json:"spool_total_bytes"`
	SpoolDiskFreeBytes      int64    `json:"spool_disk_free_bytes"`
	LastForwardSuccessAt    *string  `json:"last_forward_success_at"`
	LastForwardError        *string  `json:"last_forward_error"`
	ProxyInUse              bool     `json:"proxy_in_use"`
	UTMSourceIPs            []string `json:"utm_source_ips"`
}

type ErrorResponse struct {
	OK    bool     `json:"ok"`
	Error APIError `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
