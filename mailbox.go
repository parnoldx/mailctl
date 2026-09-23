package main

import "time"

// Msg is a message summary as returned by List.
type Msg struct {
	ID             string    `json:"id"`
	From           string    `json:"from"`
	To             []string  `json:"to"`
	Subject        string    `json:"subject"`
	Date           time.Time `json:"date"`
	Snippet        string    `json:"snippet"`
	Unread         bool      `json:"unread"`
	HasAttachments bool      `json:"has_attachments"`
}

// Attachment describes one attachment; Path is set only when it was saved to a directory.
type Attachment struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Path        string `json:"path,omitempty"` // set when saved to --save-attachments dir
}

// Full is a complete message as returned by Get.
type Full struct {
	Msg
	Headers     map[string][]string `json:"headers"`
	Text        string              `json:"text"`
	HTML        string              `json:"html"`
	Attachments []Attachment        `json:"attachments"`
}

// ListOpts narrows a List call. Zero values mean "no filter" except Limit, which defaults to 50.
type ListOpts struct {
	Folder string // "" means inbox
	Unread bool
	Since  time.Time // zero = no filter
	Limit  int       // default 50
}

// Outgoing describes a message to send.
type Outgoing struct {
	To, Cc  []string
	Subject string
	Body    string
	HTML    bool
	Attach  []string // file paths
	ReplyTo string   // message id to reply to; "" = new mail
}

// Mailbox is implemented by the IMAP and Graph backends.
type Mailbox interface {
	List(ListOpts) ([]Msg, error)
	Get(id, saveDir string) (*Full, error) // saveDir "" = don't write attachment files
	Send(Outgoing) error
	Mark(id string, read bool) error
	Move(id, folder string) error
}
