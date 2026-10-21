package wispr

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// MeetingInfo is a search_meetings item.
type MeetingInfo struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Finalized  bool      `json:"finalized"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	ModifiedAt time.Time `json:"modified_at"`
	ShareLink  string    `json:"share_link"`
	Shared     bool      `json:"shared"`
}

// MeetingPage is one page of search_meetings results (newest modified first).
type MeetingPage struct {
	Meetings   []MeetingInfo `json:"meetings"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor"`
	Skipped    int           `json:"-"` // items that could not be decoded
}

// UnmarshalJSON decodes items one by one so a single malformed meeting (for
// example an unparsable timestamp) cannot hide the rest of the page.
func (p *MeetingPage) UnmarshalJSON(b []byte) error {
	var raw struct {
		Meetings   []json.RawMessage `json:"meetings"`
		HasMore    bool              `json:"has_more"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = MeetingPage{HasMore: raw.HasMore, NextCursor: raw.NextCursor}
	for _, r := range raw.Meetings {
		var m MeetingInfo
		if json.Unmarshal(r, &m) != nil {
			p.Skipped++
			continue
		}
		p.Meetings = append(p.Meetings, m)
	}
	return nil
}

// Meeting is a get_meeting result. The transcript is never requested.
type Meeting struct {
	MeetingInfo
	Summary string            `json:"summary"`
	Todos   []json.RawMessage `json:"todos"`
}

// SearchMeetings lists meetings, most recently modified first. It deliberately
// has no date filter: Wispr's since/until select by meeting START time, which
// would miss a meeting that started earlier but was finalized later.
func (c *Client) SearchMeetings(ctx context.Context, cursor string, limit int) (MeetingPage, error) {
	args := map[string]any{"limit": limit}
	if cursor != "" {
		args["cursor"] = cursor
	}
	var page MeetingPage
	return page, c.callInto(ctx, "search_meetings", args, &page)
}

// GetMeeting fetches the summary and metadata only (no transcript, minimal notes content).
func (c *Client) GetMeeting(ctx context.Context, id string) (Meeting, error) {
	args := map[string]any{"meeting_id": id, "view_content": map[string]any{"char_limit": 1}}
	var m Meeting
	return m, c.callInto(ctx, "get_meeting", args, &m)
}

func (c *Client) callInto(ctx context.Context, tool string, args map[string]any, out any) error {
	res, err := c.CallTool(ctx, tool, args)
	if err != nil {
		return err
	}
	raw := []byte(res.StructuredContent)
	if len(raw) == 0 {
		raw = []byte(res.Text())
	}
	if len(raw) == 0 {
		return errors.New("wispr: " + tool + ": empty result")
	}
	return json.Unmarshal(raw, out)
}

// AccountName returns the display name of the signed-in Wispr user, which Wispr
// uses as the speaker label (and in summaries) for the account owner.
func (c *Client) AccountName(ctx context.Context) (string, error) {
	var out struct {
		Name string `json:"name"`
	}
	if err := c.callInto(ctx, "get_account_info", nil, &out); err != nil {
		return "", err
	}
	return out.Name, nil
}
