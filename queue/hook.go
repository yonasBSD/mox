package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/dsn"
	"github.com/mjl-/mox/message"
	"github.com/mjl-/mox/metrics"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/moxvar"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/store"
	"github.com/mjl-/mox/webhook"
	"github.com/mjl-/mox/webops"
)

var (
	metricHookRequest = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "mox_webhook_request_duration_seconds",
			Help:    "HTTP webhook call duration.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 20, 30},
		},
	)
	metricHookResult = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mox_webhook_results_total",
			Help: "HTTP webhook call results.",
		},
		[]string{"code"}, // Known http status codes (e.g. "404"), or "<major>xx" for unknown http status codes, or "error".
	)
)

// Hook is a webhook call about a delivery. We'll try delivering with backoff until we succeed or fail.
type Hook struct {
	ID         int64
	QueueMsgID int64             `bstore:"index"` // Original queue Msg/MsgRetired ID. Zero for hooks for incoming messages.
	FromID     string            // As generated by us and returned in webapi call. Can be empty, for incoming messages to our base address.
	MessageID  string            // Of outgoing or incoming messages. Includes <>.
	Subject    string            // Subject of original outgoing message, or of incoming message.
	Extra      map[string]string // From submitted message.

	Account       string `bstore:"nonzero"`
	URL           string `bstore:"nonzero"` // Taken from config when webhook is scheduled.
	Authorization string // Optional value for authorization header to include in HTTP request.
	IsIncoming    bool
	OutgoingEvent string // Empty string if not outgoing.
	Payload       string // JSON data to be submitted.

	Submitted   time.Time `bstore:"default now,index"`
	Attempts    int
	NextAttempt time.Time `bstore:"nonzero,index"` // Index for fast scheduling.
	Results     []HookResult
}

// HookResult is the result of a single attempt to deliver a webhook.
type HookResult struct {
	Start    time.Time
	Duration time.Duration
	URL      string
	Success  bool
	Code     int // eg 200, 404, 500. 2xx implies success.
	Error    string
	Response string // Max 512 bytes of HTTP response body.
}

// for logging queueing or starting delivery of a hook.
func (h Hook) attrs() []slog.Attr {
	event := string(h.OutgoingEvent)
	if h.IsIncoming {
		event = "incoming"
	}
	return []slog.Attr{
		slog.Int64("webhookid", h.ID),
		slog.Int("attempts", h.Attempts),
		slog.Int64("msgid", h.QueueMsgID),
		slog.String("account", h.Account),
		slog.String("url", h.URL),
		slog.String("fromid", h.FromID),
		slog.String("messageid", h.MessageID),
		slog.String("event", event),
		slog.Time("nextattempt", h.NextAttempt),
	}
}

// LastResult returns the last result entry, or an empty result.
func (h Hook) LastResult() HookResult {
	if len(h.Results) == 0 {
		return HookResult{}
	}
	return h.Results[len(h.Results)-1]
}

// Retired returns a HookRetired for a Hook, for insertion into the database.
func (h Hook) Retired(success bool, lastActivity, keepUntil time.Time) HookRetired {
	return HookRetired{
		ID:            h.ID,
		QueueMsgID:    h.QueueMsgID,
		FromID:        h.FromID,
		MessageID:     h.MessageID,
		Subject:       h.Subject,
		Extra:         h.Extra,
		Account:       h.Account,
		URL:           h.URL,
		Authorization: h.Authorization != "",
		IsIncoming:    h.IsIncoming,
		OutgoingEvent: h.OutgoingEvent,
		Payload:       h.Payload,
		Submitted:     h.Submitted,
		Attempts:      h.Attempts,
		Results:       h.Results,
		Success:       success,
		LastActivity:  lastActivity,
		KeepUntil:     keepUntil,
	}
}

// HookRetired is a Hook that was delivered/failed/canceled and kept according
// to the configuration.
type HookRetired struct {
	ID         int64             // Same as original Hook.ID.
	QueueMsgID int64             // Original queue Msg or MsgRetired ID. Zero for hooks for incoming messages.
	FromID     string            // As generated by us and returned in webapi call. Can be empty, for incoming messages to our base address.
	MessageID  string            // Of outgoing or incoming messages. Includes <>.
	Subject    string            // Subject of original outgoing message, or of incoming message.
	Extra      map[string]string // From submitted message.

	Account       string `bstore:"nonzero,index Account+LastActivity"`
	URL           string `bstore:"nonzero"` // Taken from config at start of each attempt.
	Authorization bool   // Whether request had authorization without keeping it around.
	IsIncoming    bool
	OutgoingEvent string
	Payload       string // JSON data submitted.

	Submitted      time.Time
	SupersededByID int64 // If not 0, a Hook.ID that superseded this one and Done will be true.
	Attempts       int
	Results        []HookResult

	Success      bool
	LastActivity time.Time `bstore:"index"`
	KeepUntil    time.Time `bstore:"index"`
}

// LastResult returns the last result entry, or an empty result.
func (h HookRetired) LastResult() HookResult {
	if len(h.Results) == 0 {
		return HookResult{}
	}
	return h.Results[len(h.Results)-1]
}

func cleanupHookRetired(done chan struct{}) {
	log := mlog.New("queue", nil)

	defer func() {
		x := recover()
		if x != nil {
			log.Error("unhandled panic while cleaning up retired webhooks", slog.Any("x", x))
			debug.PrintStack()
			metrics.PanicInc(metrics.Queue)
		}
	}()

	timer := time.NewTimer(4 * time.Second)
	for {
		select {
		case <-mox.Shutdown.Done():
			done <- struct{}{}
			return
		case <-timer.C:
		}

		cleanupHookRetiredSingle(log)
		timer.Reset(time.Hour)
	}
}

func cleanupHookRetiredSingle(log mlog.Log) {
	n, err := bstore.QueryDB[HookRetired](mox.Shutdown, DB).FilterLess("KeepUntil", time.Now()).Delete()
	log.Check(err, "removing old retired webhooks")
	if n > 0 {
		log.Debug("cleaned up retired webhooks", slog.Int("count", n))
	}
}

func hookRetiredKeep(account string) time.Duration {
	keep := 24 * 7 * time.Hour
	accConf, ok := mox.Conf.Account(account)
	if ok {
		keep = accConf.KeepRetiredWebhookPeriod
	}
	return keep
}

// HookFilter filters messages to list or operate on. Used by admin web interface
// and cli.
//
// Only non-empty/non-zero values are applied to the filter. Leaving all fields
// empty/zero matches all hooks.
type HookFilter struct {
	Max         int
	IDs         []int64
	Account     string
	Submitted   string // Whether submitted before/after a time relative to now. ">$duration" or "<$duration", also with "now" for duration.
	NextAttempt string // ">$duration" or "<$duration", also with "now" for duration.
	Event       string // Including "incoming".
}

func (f HookFilter) apply(q *bstore.Query[Hook]) error {
	if len(f.IDs) > 0 {
		q.FilterIDs(f.IDs)
	}
	applyTime := func(field string, s string) error {
		orig := s
		var less bool
		if strings.HasPrefix(s, "<") {
			less = true
		} else if !strings.HasPrefix(s, ">") {
			return fmt.Errorf(`must start with "<" for less or ">" for greater than a duration ago`)
		}
		s = strings.TrimSpace(s[1:])
		var t time.Time
		if s == "now" {
			t = time.Now()
		} else if d, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("parsing duration %q: %v", orig, err)
		} else {
			t = time.Now().Add(d)
		}
		if less {
			q.FilterLess(field, t)
		} else {
			q.FilterGreater(field, t)
		}
		return nil
	}
	if f.Submitted != "" {
		if err := applyTime("Submitted", f.Submitted); err != nil {
			return fmt.Errorf("applying filter for submitted: %v", err)
		}
	}
	if f.NextAttempt != "" {
		if err := applyTime("NextAttempt", f.NextAttempt); err != nil {
			return fmt.Errorf("applying filter for next attempt: %v", err)
		}
	}
	if f.Account != "" {
		q.FilterNonzero(Hook{Account: f.Account})
	}
	if f.Event != "" {
		if f.Event == "incoming" {
			q.FilterNonzero(Hook{IsIncoming: true})
		} else {
			q.FilterNonzero(Hook{OutgoingEvent: f.Event})
		}
	}
	if f.Max != 0 {
		q.Limit(f.Max)
	}
	return nil
}

type HookSort struct {
	Field  string // "Queued" or "NextAttempt"/"".
	LastID int64  // If > 0, we return objects beyond this, less/greater depending on Asc.
	Last   any    // Value of Field for last object. Must be set iff LastID is set.
	Asc    bool   // Ascending, or descending.
}

func (s HookSort) apply(q *bstore.Query[Hook]) error {
	switch s.Field {
	case "", "NextAttempt":
		s.Field = "NextAttempt"
	case "Submitted":
		s.Field = "Submitted"
	default:
		return fmt.Errorf("unknown sort order field %q", s.Field)
	}

	if s.LastID > 0 {
		ls, ok := s.Last.(string)
		if !ok {
			return fmt.Errorf("last should be string with time, not %T %q", s.Last, s.Last)
		}
		last, err := time.Parse(time.RFC3339Nano, ls)
		if err != nil {
			last, err = time.Parse(time.RFC3339, ls)
		}
		if err != nil {
			return fmt.Errorf("parsing last %q as time: %v", s.Last, err)
		}
		q.FilterNotEqual("ID", s.LastID)
		var fieldEqual func(h Hook) bool
		if s.Field == "NextAttempt" {
			fieldEqual = func(h Hook) bool { return h.NextAttempt.Equal(last) }
		} else {
			fieldEqual = func(h Hook) bool { return h.Submitted.Equal(last) }
		}
		if s.Asc {
			q.FilterGreaterEqual(s.Field, last)
			q.FilterFn(func(h Hook) bool {
				return !fieldEqual(h) || h.ID > s.LastID
			})
		} else {
			q.FilterLessEqual(s.Field, last)
			q.FilterFn(func(h Hook) bool {
				return !fieldEqual(h) || h.ID < s.LastID
			})
		}
	}
	if s.Asc {
		q.SortAsc(s.Field, "ID")
	} else {
		q.SortDesc(s.Field, "ID")
	}
	return nil
}

// HookQueueSize returns the number of webhooks in the queue.
func HookQueueSize(ctx context.Context) (int, error) {
	return bstore.QueryDB[Hook](ctx, DB).Count()
}

// HookList returns webhooks according to filter and sort.
func HookList(ctx context.Context, filter HookFilter, sort HookSort) ([]Hook, error) {
	q := bstore.QueryDB[Hook](ctx, DB)
	if err := filter.apply(q); err != nil {
		return nil, err
	}
	if err := sort.apply(q); err != nil {
		return nil, err
	}
	return q.List()
}

// HookRetiredFilter filters messages to list or operate on. Used by admin web interface
// and cli.
//
// Only non-empty/non-zero values are applied to the filter. Leaving all fields
// empty/zero matches all hooks.
type HookRetiredFilter struct {
	Max          int
	IDs          []int64
	Account      string
	Submitted    string // Whether submitted before/after a time relative to now. ">$duration" or "<$duration", also with "now" for duration.
	LastActivity string // ">$duration" or "<$duration", also with "now" for duration.
	Event        string // Including "incoming".
}

func (f HookRetiredFilter) apply(q *bstore.Query[HookRetired]) error {
	if len(f.IDs) > 0 {
		q.FilterIDs(f.IDs)
	}
	applyTime := func(field string, s string) error {
		orig := s
		var less bool
		if strings.HasPrefix(s, "<") {
			less = true
		} else if !strings.HasPrefix(s, ">") {
			return fmt.Errorf(`must start with "<" for before or ">" for after a duration`)
		}
		s = strings.TrimSpace(s[1:])
		var t time.Time
		if s == "now" {
			t = time.Now()
		} else if d, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("parsing duration %q: %v", orig, err)
		} else {
			t = time.Now().Add(d)
		}
		if less {
			q.FilterLess(field, t)
		} else {
			q.FilterGreater(field, t)
		}
		return nil
	}
	if f.Submitted != "" {
		if err := applyTime("Submitted", f.Submitted); err != nil {
			return fmt.Errorf("applying filter for submitted: %v", err)
		}
	}
	if f.LastActivity != "" {
		if err := applyTime("LastActivity", f.LastActivity); err != nil {
			return fmt.Errorf("applying filter for last activity: %v", err)
		}
	}
	if f.Account != "" {
		q.FilterNonzero(HookRetired{Account: f.Account})
	}
	if f.Event != "" {
		if f.Event == "incoming" {
			q.FilterNonzero(HookRetired{IsIncoming: true})
		} else {
			q.FilterNonzero(HookRetired{OutgoingEvent: f.Event})
		}
	}
	if f.Max != 0 {
		q.Limit(f.Max)
	}
	return nil
}

type HookRetiredSort struct {
	Field  string // "Queued" or "LastActivity"/"".
	LastID int64  // If > 0, we return objects beyond this, less/greater depending on Asc.
	Last   any    // Value of Field for last object. Must be set iff LastID is set.
	Asc    bool   // Ascending, or descending.
}

func (s HookRetiredSort) apply(q *bstore.Query[HookRetired]) error {
	switch s.Field {
	case "", "LastActivity":
		s.Field = "LastActivity"
	case "Submitted":
		s.Field = "Submitted"
	default:
		return fmt.Errorf("unknown sort order field %q", s.Field)
	}

	if s.LastID > 0 {
		ls, ok := s.Last.(string)
		if !ok {
			return fmt.Errorf("last should be string with time, not %T %q", s.Last, s.Last)
		}
		last, err := time.Parse(time.RFC3339Nano, ls)
		if err != nil {
			last, err = time.Parse(time.RFC3339, ls)
		}
		if err != nil {
			return fmt.Errorf("parsing last %q as time: %v", s.Last, err)
		}
		q.FilterNotEqual("ID", s.LastID)
		var fieldEqual func(hr HookRetired) bool
		if s.Field == "LastActivity" {
			fieldEqual = func(hr HookRetired) bool { return hr.LastActivity.Equal(last) }
		} else {
			fieldEqual = func(hr HookRetired) bool { return hr.Submitted.Equal(last) }
		}
		if s.Asc {
			q.FilterGreaterEqual(s.Field, last)
			q.FilterFn(func(hr HookRetired) bool {
				return !fieldEqual(hr) || hr.ID > s.LastID
			})
		} else {
			q.FilterLessEqual(s.Field, last)
			q.FilterFn(func(hr HookRetired) bool {
				return !fieldEqual(hr) || hr.ID < s.LastID
			})
		}
	}
	if s.Asc {
		q.SortAsc(s.Field, "ID")
	} else {
		q.SortDesc(s.Field, "ID")
	}
	return nil
}

// HookRetiredList returns retired webhooks according to filter and sort.
func HookRetiredList(ctx context.Context, filter HookRetiredFilter, sort HookRetiredSort) ([]HookRetired, error) {
	q := bstore.QueryDB[HookRetired](ctx, DB)
	if err := filter.apply(q); err != nil {
		return nil, err
	}
	if err := sort.apply(q); err != nil {
		return nil, err
	}
	return q.List()
}

// HookNextAttemptAdd adds a duration to the NextAttempt for all matching messages, and
// kicks the queue.
func HookNextAttemptAdd(ctx context.Context, filter HookFilter, d time.Duration) (affected int, err error) {
	err = DB.Write(ctx, func(tx *bstore.Tx) error {
		q := bstore.QueryTx[Hook](tx)
		if err := filter.apply(q); err != nil {
			return err
		}
		hooks, err := q.List()
		if err != nil {
			return fmt.Errorf("listing matching hooks: %v", err)
		}
		for _, h := range hooks {
			h.NextAttempt = h.NextAttempt.Add(d)
			if err := tx.Update(&h); err != nil {
				return err
			}
		}
		affected = len(hooks)
		return nil
	})
	if err != nil {
		return 0, err
	}
	hookqueueKick()
	return affected, nil
}

// HookNextAttemptSet sets NextAttempt for all matching messages to a new absolute
// time and kicks the queue.
func HookNextAttemptSet(ctx context.Context, filter HookFilter, t time.Time) (affected int, err error) {
	q := bstore.QueryDB[Hook](ctx, DB)
	if err := filter.apply(q); err != nil {
		return 0, err
	}
	n, err := q.UpdateNonzero(Hook{NextAttempt: t})
	if err != nil {
		return 0, fmt.Errorf("selecting and updating hooks in queue: %v", err)
	}
	hookqueueKick()
	return n, nil
}

// HookCancel prevents more delivery attempts of the hook, moving it to the
// retired list if configured.
func HookCancel(ctx context.Context, log mlog.Log, filter HookFilter) (affected int, err error) {
	var hooks []Hook
	err = DB.Write(ctx, func(tx *bstore.Tx) error {
		q := bstore.QueryTx[Hook](tx)
		if err := filter.apply(q); err != nil {
			return err
		}
		q.Gather(&hooks)
		n, err := q.Delete()
		if err != nil {
			return fmt.Errorf("selecting and deleting hooks from queue: %v", err)
		}

		if len(hooks) == 0 {
			return nil
		}

		now := time.Now()
		for _, h := range hooks {
			keep := hookRetiredKeep(h.Account)
			if keep > 0 {
				hr := h.Retired(false, now, now.Add(keep))
				hr.Results = append(hr.Results, HookResult{Start: now, Error: "canceled by admin"})
				if err := tx.Insert(&hr); err != nil {
					return fmt.Errorf("inserting retired hook: %v", err)
				}
			}
		}

		affected = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, h := range hooks {
		log.Info("canceled hook", h.attrs()...)
	}
	hookqueueKick()
	return affected, nil
}

func hookCompose(m Msg, url, authz string, event webhook.OutgoingEvent, suppressing bool, code int, secodeOpt string) (Hook, error) {
	now := time.Now()

	var lastError string
	if len(m.Results) > 0 {
		lastError = m.Results[len(m.Results)-1].Error
	}
	var ecode string
	if secodeOpt != "" {
		ecode = fmt.Sprintf("%d.%s", code/100, secodeOpt)
	}
	data := webhook.Outgoing{
		Event:            event,
		Suppressing:      suppressing,
		QueueMsgID:       m.ID,
		FromID:           m.FromID,
		MessageID:        m.MessageID,
		Subject:          m.Subject,
		WebhookQueued:    now,
		Error:            lastError,
		SMTPCode:         code,
		SMTPEnhancedCode: ecode,
		Extra:            m.Extra,
	}
	if data.Extra == nil {
		data.Extra = map[string]string{}
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return Hook{}, fmt.Errorf("marshal webhook payload: %v", err)
	}

	h := Hook{
		QueueMsgID:    m.ID,
		FromID:        m.FromID,
		MessageID:     m.MessageID,
		Subject:       m.Subject,
		Extra:         m.Extra,
		Account:       m.SenderAccount,
		URL:           url,
		Authorization: authz,
		IsIncoming:    false,
		OutgoingEvent: string(event),
		Payload:       string(payload),
		Submitted:     now,
		NextAttempt:   now,
	}
	return h, nil
}

// Incoming processes a message delivered over SMTP for webhooks. If the message is
// a DSN, a webhook for outgoing deliveries may be scheduled (if configured).
// Otherwise, a webhook for incoming deliveries may be scheduled.
func Incoming(ctx context.Context, log mlog.Log, acc *store.Account, messageID string, m store.Message, part message.Part, mailboxName string) error {
	now := time.Now()
	var data any

	log = log.With(
		slog.Int64("msgid", m.ID),
		slog.String("messageid", messageID),
		slog.String("mailbox", mailboxName),
	)

	// todo future: if there is no fromid in our rcpt address, but this is a 3-part dsn with headers that includes message-id, try matching based on that.
	// todo future: once we implement the SMTP DSN extension, use ENVID when sending (if destination implements it), and start looking for Original-Envelope-ID in the DSN.

	// If this is a DSN for a message we sent, don't deliver a hook for incoming
	// message, but an outgoing status webhook.
	var fromID string
	dom, err := dns.ParseDomain(m.RcptToDomain)
	if err != nil {
		log.Debugx("parsing recipient domain in incoming message", err)
	} else {
		domconf, _ := mox.Conf.Domain(dom)
		if len(domconf.LocalpartCatchallSeparatorsEffective) > 0 {
			t := strings.SplitN(string(m.RcptToLocalpart), domconf.LocalpartCatchallSeparatorsEffective[0], 2)
			if len(t) == 2 {
				fromID = t[1]
			}
		}
	}
	var outgoingEvent webhook.OutgoingEvent
	var queueMsgID int64
	var subject string
	if fromID != "" {
		err := DB.Write(ctx, func(tx *bstore.Tx) (rerr error) {
			mr, err := bstore.QueryTx[MsgRetired](tx).FilterNonzero(MsgRetired{FromID: fromID}).Get()
			if err == bstore.ErrAbsent {
				log.Debug("no original message found for fromid", slog.String("fromid", fromID))
				return nil
			} else if err != nil {
				return fmt.Errorf("looking up original message for fromid: %v", err)
			}

			queueMsgID = mr.ID
			subject = mr.Subject

			log = log.With(slog.String("fromid", fromID))
			log.Debug("processing incoming message about previous delivery for webhooks")

			// We'll record this message in the results.
			mr.LastActivity = now
			mr.Results = append(mr.Results, MsgResult{Start: now, Error: "incoming message"})
			result := &mr.Results[len(mr.Results)-1] // Updated below.

			outgoingEvent = webhook.EventUnrecognized
			var suppressedMsgIDs []int64
			var isDSN bool
			var code int
			var secode string
			defer func() {
				if rerr == nil {
					var ecode string
					if secode != "" {
						ecode = fmt.Sprintf("%d.%s", code/100, secode)
					}
					data = webhook.Outgoing{
						Event:            outgoingEvent,
						DSN:              isDSN,
						Suppressing:      len(suppressedMsgIDs) > 0,
						QueueMsgID:       mr.ID,
						FromID:           fromID,
						MessageID:        mr.MessageID,
						Subject:          mr.Subject,
						WebhookQueued:    now,
						SMTPCode:         code,
						SMTPEnhancedCode: ecode,
						Extra:            mr.Extra,
					}

					if err := tx.Update(&mr); err != nil {
						rerr = fmt.Errorf("updating retired message after processing: %v", err)
						return
					}
				}
			}()

			if !(part.MediaType == "MULTIPART" && part.MediaSubType == "REPORT" && len(part.Parts) >= 2 && part.Parts[1].MediaType == "MESSAGE" && (part.Parts[1].MediaSubType == "DELIVERY-STATUS" || part.Parts[1].MediaSubType == "GLOBAL-DELIVERY-STATUS")) {
				// Some kind of delivery-related event, but we don't recognize it.
				result.Error = "incoming message not a dsn"
				return nil
			}
			isDSN = true
			dsnutf8 := part.Parts[1].MediaSubType == "GLOBAL-DELIVERY-STATUS"
			dsnmsg, err := dsn.Decode(part.Parts[1].ReaderUTF8OrBinary(), dsnutf8)
			if err != nil {
				log.Infox("parsing dsn message for webhook", err)
				result.Error = fmt.Sprintf("parsing incoming dsn: %v", err)
				return nil
			} else if len(dsnmsg.Recipients) != 1 {
				log.Info("dsn message for webhook does not have exactly one dsn recipient", slog.Int("nrecipients", len(dsnmsg.Recipients)))
				result.Error = fmt.Sprintf("incoming dsn has %d recipients, expecting 1", len(dsnmsg.Recipients))
				return nil
			}

			dsnrcpt := dsnmsg.Recipients[0]

			if dsnrcpt.DiagnosticCodeSMTP != "" {
				code, secode = parseSMTPCodes(dsnrcpt.DiagnosticCodeSMTP)
			}
			if code == 0 && dsnrcpt.Status != "" {
				if strings.HasPrefix(dsnrcpt.Status, "4.") {
					code = 400
					secode = dsnrcpt.Status[2:]
				} else if strings.HasPrefix(dsnrcpt.Status, "5.") {
					code = 500
					secode = dsnrcpt.Status[2:]
				}
			}
			result.Code = code
			result.Secode = secode
			log.Debug("incoming dsn message", slog.String("action", string(dsnrcpt.Action)), slog.Int("dsncode", code), slog.String("dsnsecode", secode))

			switch s := dsnrcpt.Action; s {
			case dsn.Failed:
				outgoingEvent = webhook.EventFailed

				if code != 0 {
					sc := suppressionCheck{
						MsgID:     mr.ID,
						Account:   acc.Name,
						Recipient: mr.Recipient(),
						Code:      code,
						Secode:    secode,
						Source:    "DSN",
					}
					suppressedMsgIDs, err = suppressionProcess(log, tx, sc)
					if err != nil {
						return fmt.Errorf("processing dsn for suppression list: %v", err)
					}
				} else {
					log.Debug("no code/secode in dsn for failed delivery", slog.Int64("msgid", mr.ID))
				}

			case dsn.Delayed, dsn.Delivered, dsn.Relayed, dsn.Expanded:
				outgoingEvent = webhook.OutgoingEvent(string(s))
				result.Success = s != dsn.Delayed

			default:
				log.Info("unrecognized dsn action", slog.String("action", string(dsnrcpt.Action)))
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("processing message based on fromid: %v", err)
		}
	}

	accConf, _ := acc.Conf()

	var hookURL, authz string
	var isIncoming bool
	if data == nil {
		if accConf.IncomingWebhook == nil {
			return nil
		}
		hookURL = accConf.IncomingWebhook.URL
		authz = accConf.IncomingWebhook.Authorization

		log.Debug("composing webhook for incoming message")

		structure, err := PartStructure(log, &part)
		if err != nil {
			return fmt.Errorf("parsing part structure: %v", err)
		}

		isIncoming = true
		var rcptTo string
		if m.RcptToDomain != "" {
			rcptTo = m.RcptToLocalpart.String() + "@" + m.RcptToDomain
		}
		in := webhook.Incoming{
			Structure: structure,
			Meta: webhook.IncomingMeta{
				MsgID:               m.ID,
				MailFrom:            m.MailFrom,
				MailFromValidated:   m.MailFromValidated,
				MsgFromValidated:    m.MsgFromValidated,
				RcptTo:              rcptTo,
				DKIMVerifiedDomains: m.DKIMDomains,
				RemoteIP:            m.RemoteIP,
				Received:            m.Received,
				MailboxName:         mailboxName,
			},
		}
		if in.Meta.DKIMVerifiedDomains == nil {
			in.Meta.DKIMVerifiedDomains = []string{}
		}
		if env := part.Envelope; env != nil {
			subject = env.Subject
			in.From = addresses(env.From)
			in.To = addresses(env.To)
			in.CC = addresses(env.CC)
			in.BCC = addresses(env.BCC)
			in.ReplyTo = addresses(env.ReplyTo)
			in.Subject = env.Subject
			in.MessageID = env.MessageID
			in.InReplyTo = env.InReplyTo
			if !env.Date.IsZero() {
				in.Date = &env.Date
			}
		}
		// todo: ideally, we would have this information available in parsed Part, not require parsing headers here.
		h, err := part.Header()
		if err != nil {
			log.Debugx("parsing headers of incoming message", err, slog.Int64("msgid", m.ID))
		} else {
			refs, err := message.ReferencedIDs(h.Values("References"), nil)
			if err != nil {
				log.Debugx("parsing references header", err, slog.Int64("msgid", m.ID))
			}
			for i, r := range refs {
				refs[i] = "<" + r + ">"
			}
			if refs == nil {
				refs = []string{}
			}
			in.References = refs

			// Check if message is automated. Empty SMTP MAIL FROM indicates this was some kind
			// of service message. Several headers indicate out-of-office replies, messages
			// from mailing or marketing lists. And the content-type can indicate a report
			// (e.g. DSN/MDN).
			in.Meta.Automated = m.MailFrom == "" || isAutomated(h) || part.MediaType == "MULTIPART" && part.MediaSubType == "REPORT"
		}

		text, html, _, err := webops.ReadableParts(part, 1*1024*1024)
		if err != nil {
			log.Debugx("looking for text and html content in message", err)
		}
		in.Text = strings.ReplaceAll(text, "\r\n", "\n")
		in.HTML = strings.ReplaceAll(html, "\r\n", "\n")

		data = in
	} else if accConf.OutgoingWebhook == nil {
		return nil
	} else if len(accConf.OutgoingWebhook.Events) == 0 || slices.Contains(accConf.OutgoingWebhook.Events, string(outgoingEvent)) {
		hookURL = accConf.OutgoingWebhook.URL
		authz = accConf.OutgoingWebhook.Authorization
	} else {
		log.Debug("not sending webhook, account not subscribed for event", slog.String("event", string(outgoingEvent)))
		return nil
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %v", err)
	}

	h := Hook{
		QueueMsgID:    queueMsgID,
		FromID:        fromID,
		MessageID:     messageID,
		Subject:       subject,
		Account:       acc.Name,
		URL:           hookURL,
		Authorization: authz,
		IsIncoming:    isIncoming,
		OutgoingEvent: string(outgoingEvent),
		Payload:       string(payload),
		Submitted:     now,
		NextAttempt:   now,
	}
	err = DB.Write(ctx, func(tx *bstore.Tx) error {
		if err := hookInsert(tx, &h, now, accConf.KeepRetiredWebhookPeriod); err != nil {
			return fmt.Errorf("queueing webhook for incoming message: %v", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inserting webhook in database: %v", err)
	}
	log.Debug("queued webhook for incoming message", h.attrs()...)
	hookqueueKick()
	return nil
}

// PartStructure returns a webhook.Structure for a parsed message part.
func PartStructure(log mlog.Log, p *message.Part) (webhook.Structure, error) {
	parts := make([]webhook.Structure, len(p.Parts))
	for i := range p.Parts {
		var err error
		parts[i], err = PartStructure(log, &p.Parts[i])
		if err != nil && !errors.Is(err, message.ErrParamEncoding) {
			return webhook.Structure{}, err
		}
	}
	disp, filename, err := p.DispositionFilename()
	if err != nil && errors.Is(err, message.ErrParamEncoding) {
		log.Debugx("parsing disposition/filename", err)
	} else if err != nil {
		return webhook.Structure{}, err
	}
	var cid string
	if p.ContentID != nil {
		cid = *p.ContentID
	}
	s := webhook.Structure{
		ContentType:        strings.ToLower(p.MediaType + "/" + p.MediaSubType),
		ContentTypeParams:  p.ContentTypeParams,
		ContentID:          cid,
		ContentDisposition: strings.ToLower(disp),
		Filename:           filename,
		DecodedSize:        p.DecodedSize,
		Parts:              parts,
	}
	// Replace nil map with empty map, for easier to use JSON.
	if s.ContentTypeParams == nil {
		s.ContentTypeParams = map[string]string{}
	}
	return s, nil
}

func isAutomated(h textproto.MIMEHeader) bool {
	l := []string{"List-Id", "List-Unsubscribe", "List-Unsubscribe-Post", "Precedence"}
	for _, k := range l {
		if h.Get(k) != "" {
			return true
		}
	}
	if s := strings.TrimSpace(h.Get("Auto-Submitted")); s != "" && !strings.EqualFold(s, "no") {
		return true
	}
	return false
}

func parseSMTPCodes(line string) (code int, secode string) {
	t := strings.SplitN(line, " ", 3)
	if len(t) <= 1 || len(t[0]) != 3 {
		return 0, ""
	}
	v, err := strconv.ParseUint(t[0], 10, 64)
	if err != nil || code >= 600 {
		return 0, ""
	}
	if len(t) >= 2 && (strings.HasPrefix(t[1], "4.") || strings.HasPrefix(t[1], "5.")) {
		secode = t[1][2:]
	}
	return int(v), secode
}

// Insert hook into database, but first retire any existing pending hook for
// QueueMsgID if it is > 0.
func hookInsert(tx *bstore.Tx, h *Hook, now time.Time, accountKeepPeriod time.Duration) error {
	if err := tx.Insert(h); err != nil {
		return fmt.Errorf("insert webhook: %v", err)
	}
	if h.QueueMsgID == 0 {
		return nil
	}

	// Find existing queued hook for previously msgid from queue. Can be at most one.
	oh, err := bstore.QueryTx[Hook](tx).FilterNonzero(Hook{QueueMsgID: h.QueueMsgID}).FilterNotEqual("ID", h.ID).Get()
	if err == bstore.ErrAbsent {
		return nil
	} else if err != nil {
		return fmt.Errorf("get existing webhook before inserting new hook for same queuemsgid %d", h.QueueMsgID)
	}

	// Retire this queued hook.
	// This hook may be in the process of being delivered. When delivered, we'll try to
	// move it from Hook to HookRetired. But that will fail since Hook is already
	// retired. We detect that situation and update the retired hook with the new
	// (final) result.
	if accountKeepPeriod > 0 {
		hr := oh.Retired(false, now, now.Add(accountKeepPeriod))
		hr.SupersededByID = h.ID
		if err := tx.Insert(&hr); err != nil {
			return fmt.Errorf("inserting superseded webhook as retired hook: %v", err)
		}
	}
	if err := tx.Delete(&oh); err != nil {
		return fmt.Errorf("deleting superseded webhook: %v", err)
	}
	return nil
}

func addresses(al []message.Address) []webhook.NameAddress {
	l := make([]webhook.NameAddress, len(al))
	for i, a := range al {
		addr := a.User + "@" + a.Host
		pa, err := smtp.ParseAddress(addr)
		if err == nil {
			addr = pa.Pack(true)
		}
		l[i] = webhook.NameAddress{
			Name:    a.Name,
			Address: addr,
		}
	}
	return l
}

var (
	hookqueue           = make(chan struct{}, 1)
	hookDeliveryResults = make(chan string, 1)
)

func hookqueueKick() {
	select {
	case hookqueue <- struct{}{}:
	default:
	}
}

func startHookQueue(done chan struct{}) {
	log := mlog.New("queue", nil)
	busyHookURLs := map[string]struct{}{}
	timer := time.NewTimer(0)
	for {
		select {
		case <-mox.Shutdown.Done():
			for len(busyHookURLs) > 0 {
				url := <-hookDeliveryResults
				delete(busyHookURLs, url)
			}
			done <- struct{}{}
			return
		case <-hookqueue:
		case <-timer.C:
		case url := <-hookDeliveryResults:
			delete(busyHookURLs, url)
		}

		if len(busyHookURLs) >= maxConcurrentHookDeliveries {
			continue
		}

		hookLaunchWork(log, busyHookURLs)
		timer.Reset(hookNextWork(mox.Shutdown, log, busyHookURLs))
	}
}

func hookNextWork(ctx context.Context, log mlog.Log, busyURLs map[string]struct{}) time.Duration {
	q := bstore.QueryDB[Hook](ctx, DB)
	if len(busyURLs) > 0 {
		var urls []any
		for u := range busyURLs {
			urls = append(urls, u)
		}
		q.FilterNotEqual("URL", urls...)
	}
	q.SortAsc("NextAttempt")
	q.Limit(1)
	h, err := q.Get()
	if err == bstore.ErrAbsent {
		return 24 * time.Hour
	} else if err != nil {
		log.Errorx("finding time for next webhook delivery attempt", err)
		return 1 * time.Minute
	}
	return time.Until(h.NextAttempt)
}

func hookLaunchWork(log mlog.Log, busyURLs map[string]struct{}) int {
	q := bstore.QueryDB[Hook](mox.Shutdown, DB)
	q.FilterLessEqual("NextAttempt", time.Now())
	q.SortAsc("NextAttempt")
	q.Limit(maxConcurrentHookDeliveries)
	if len(busyURLs) > 0 {
		var urls []any
		for u := range busyURLs {
			urls = append(urls, u)
		}
		q.FilterNotEqual("URL", urls...)
	}
	var hooks []Hook
	seen := map[string]bool{}
	err := q.ForEach(func(h Hook) error {
		u := h.URL
		if _, ok := busyURLs[u]; !ok && !seen[u] {
			seen[u] = true
			hooks = append(hooks, h)
		}
		return nil
	})
	if err != nil {
		log.Errorx("querying for work in webhook queue", err)
		mox.Sleep(mox.Shutdown, 1*time.Second)
		return -1
	}

	for _, h := range hooks {
		busyURLs[h.URL] = struct{}{}
		go hookDeliver(log, h)
	}
	return len(hooks)
}

var hookIntervals []time.Duration

func init() {
	const M = time.Minute
	const H = time.Hour
	hookIntervals = []time.Duration{M, 2 * M, 4 * M, 15 * M / 2, 15 * M, 30 * M, 1 * H, 2 * H, 4 * H, 8 * H, 16 * H}
}

func hookDeliver(log mlog.Log, h Hook) {
	ctx := mox.Shutdown

	qlog := log.WithCid(mox.Cid())
	qlog.Debug("attempting to deliver webhook", h.attrs()...)
	qlog = qlog.With(slog.Int64("webhookid", h.ID))

	defer func() {
		hookDeliveryResults <- h.URL

		x := recover()
		if x != nil {
			qlog.Error("webhook deliver panic", slog.Any("panic", x))
			debug.PrintStack()
			metrics.PanicInc(metrics.Queue)
		}
	}()

	// todo: should we get a new webhook url from the config before attempting? would intervene with our "urls busy" approach. may not be worth it.

	// Set Attempts & NextAttempt early. In case of failures while processing, at least
	// we won't try again immediately. We do backoff at intervals:
	var backoff time.Duration
	if h.Attempts < len(hookIntervals) {
		backoff = hookIntervals[h.Attempts]
	} else {
		backoff = hookIntervals[len(hookIntervals)-1] * time.Duration(2)
	}
	backoff += time.Duration(jitter.IntN(200)-100) * backoff / 10000
	h.Attempts++
	now := time.Now()
	h.NextAttempt = now.Add(backoff)
	h.Results = append(h.Results, HookResult{Start: now, URL: h.URL, Error: resultErrorDelivering})
	result := &h.Results[len(h.Results)-1]
	if err := DB.Update(mox.Shutdown, &h); err != nil {
		qlog.Errorx("storing webhook delivery attempt", err)
		return
	}

	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t0 := time.Now()
	code, response, err := HookPost(hctx, qlog, h.ID, h.Attempts, h.URL, h.Authorization, h.Payload)
	result.Duration = time.Since(t0)
	result.Success = err == nil
	result.Code = code
	result.Error = ""
	result.Response = response
	if err != nil {
		result.Error = err.Error()
	}
	if err != nil && h.Attempts <= len(hookIntervals) {
		// We'll try again later, so only update existing record.
		qlog.Debugx("webhook delivery failed, will try again later", err)
		xerr := DB.Write(context.Background(), func(tx *bstore.Tx) error {
			if err := tx.Update(&h); err == bstore.ErrAbsent {
				return updateRetiredHook(tx, h, result)
			} else if err != nil {
				return fmt.Errorf("update webhook after retryable failure: %v", err)
			}
			return nil
		})
		qlog.Check(xerr, "updating failed webhook delivery attempt in database", slog.String("deliveryerr", err.Error()))
		return
	}

	qlog.Debugx("webhook delivery completed", err, slog.Bool("success", result.Success))

	// Move Hook to HookRetired.
	err = DB.Write(context.Background(), func(tx *bstore.Tx) error {
		if err := tx.Delete(&h); err == bstore.ErrAbsent {
			return updateRetiredHook(tx, h, result)
		} else if err != nil {
			return fmt.Errorf("removing webhook from database: %v", err)
		}
		keep := hookRetiredKeep(h.Account)
		if keep > 0 {
			hr := h.Retired(result.Success, t0, t0.Add(keep))
			if err := tx.Insert(&hr); err != nil {
				return fmt.Errorf("inserting retired webhook in database: %v", err)
			}
		}
		return nil
	})
	qlog.Check(err, "moving delivered webhook from to retired hooks")
}

func updateRetiredHook(tx *bstore.Tx, h Hook, result *HookResult) error {
	// Hook is gone. It may have been superseded and moved to HookRetired while we were
	// delivering it. If so, add the result to the retired hook.
	hr := HookRetired{ID: h.ID}
	if err := tx.Get(&hr); err != nil {
		return fmt.Errorf("result for webhook that was no longer in webhook queue or retired webhooks: %v", err)
	}
	result.Error += "(superseded)"
	hr.Results = append(hr.Results, *result)
	if err := tx.Update(&hr); err != nil {
		return fmt.Errorf("updating retired webhook after webhook was superseded during delivery: %v", err)
	}
	return nil
}

var hookClient = &http.Client{Transport: hookTransport()}

func hookTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// Limit resources consumed during idle periods, probably most of the time. But
	// during busy periods, we may use the few connections for many events.
	t.IdleConnTimeout = 5 * time.Second
	t.MaxIdleConnsPerHost = 2
	return t
}

func HookPost(ctx context.Context, log mlog.Log, hookID int64, attempt int, url, authz string, payload string) (code int, response string, err error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(payload))
	if err != nil {
		return 0, "", fmt.Errorf("new request: %v", err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("mox/%s (webhook)", moxvar.Version))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-Mox-Webhook-ID", fmt.Sprintf("%d", hookID))
	req.Header.Set("X-Mox-Webhook-Attempt", fmt.Sprintf("%d", attempt))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	t0 := time.Now()
	resp, err := hookClient.Do(req)
	metricHookRequest.Observe(float64(time.Since(t0)) / float64(time.Second))
	if err != nil {
		metricHookResult.WithLabelValues("error").Inc()
		log.Debugx("webhook http transaction", err)
		return 0, "", fmt.Errorf("http transact: %v", err)
	}
	defer func() {
		err := resp.Body.Close()
		log.Check(err, "closing response body")
	}()

	// Use full http status code for known codes, and a generic "<major>xx" for others.
	result := fmt.Sprintf("%d", resp.StatusCode)
	if http.StatusText(resp.StatusCode) == "" {
		result = fmt.Sprintf("%dxx", resp.StatusCode/100)
	}
	metricHookResult.WithLabelValues(result).Inc()
	log.Debug("webhook http post result", slog.Int("statuscode", resp.StatusCode), slog.Duration("duration", time.Since(t0)))

	respbuf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("http status %q, expected 200 ok", resp.Status)
	}
	return resp.StatusCode, string(respbuf), err
}
