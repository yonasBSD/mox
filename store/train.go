package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/config"
	"github.com/mjl-/mox/junk"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
)

// ErrNoJunkFilter indicates user did not configure/enable a junk filter.
var ErrNoJunkFilter = errors.New("junkfilter: not configured")

func (a *Account) HasJunkFilter() bool {
	conf, _ := a.Conf()
	return conf.JunkFilter != nil
}

// OpenJunkFilter returns an opened junk filter for the account.
// If the account does not have a junk filter enabled, ErrNotConfigured is returned.
// Do not forget to save the filter after modifying, and to always close the filter when done.
// An empty filter is initialized on first access of the filter.
func (a *Account) OpenJunkFilter(ctx context.Context, log mlog.Log) (*junk.Filter, *config.JunkFilter, error) {
	conf, ok := a.Conf()
	if !ok {
		return nil, nil, ErrAccountUnknown
	}
	jf := conf.JunkFilter
	if jf == nil {
		return nil, jf, ErrNoJunkFilter
	}

	basePath := mox.DataDirPath("accounts")
	dbPath := filepath.Join(basePath, a.Name, "junkfilter.db")
	bloomPath := filepath.Join(basePath, a.Name, "junkfilter.bloom")

	if _, xerr := os.Stat(dbPath); xerr != nil && os.IsNotExist(xerr) {
		f, err := junk.NewFilter(ctx, log, jf.Params, dbPath, bloomPath)
		return f, jf, err
	}
	f, err := junk.OpenFilter(ctx, log, jf.Params, dbPath, bloomPath, false)
	return f, jf, err
}

func (a *Account) ensureJunkFilter(ctx context.Context, log mlog.Log, jfOpt *junk.Filter) (jf *junk.Filter, opened bool, err error) {
	if jfOpt != nil {
		return jfOpt, false, nil
	}

	jf, _, err = a.OpenJunkFilter(ctx, log)
	if err != nil {
		return nil, false, fmt.Errorf("open junk filter: %v", err)
	}
	return jf, true, nil
}

// RetrainMessages (un)trains messages, if relevant given their flags. Updates
// m.TrainedJunk after retraining.
func (a *Account) RetrainMessages(ctx context.Context, log mlog.Log, tx *bstore.Tx, msgs []Message) (rerr error) {
	if len(msgs) == 0 {
		return nil
	}

	var jf *junk.Filter

	for i := range msgs {
		if !msgs[i].NeedsTraining() {
			continue
		}

		// Lazy open the junk filter.
		if jf == nil {
			var err error
			jf, _, err = a.OpenJunkFilter(ctx, log)
			if err != nil && errors.Is(err, ErrNoJunkFilter) {
				// No junk filter configured. Nothing more to do.
				return nil
			} else if err != nil {
				return fmt.Errorf("open junk filter: %v", err)
			}
			defer func() {
				if rerr != nil {
					err := jf.CloseDiscard()
					log.Check(err, "close junk filter without saving")
				} else {
					rerr = jf.Close()
				}
			}()
		}
		if err := a.RetrainMessage(ctx, log, tx, jf, &msgs[i]); err != nil {
			return err
		}
	}
	return nil
}

// RetrainMessage untrains and/or trains a message, if relevant given m.TrainedJunk
// and m.Junk/m.Notjunk. Updates m.TrainedJunk after retraining.
func (a *Account) RetrainMessage(ctx context.Context, log mlog.Log, tx *bstore.Tx, jf *junk.Filter, m *Message) error {
	need, untrain, untrainJunk, train, trainJunk := m.needsTraining()
	if !need {
		return nil
	}
	log.Debug("updating junk filter",
		slog.Bool("untrain", untrain),
		slog.Bool("untrainjunk", untrainJunk),
		slog.Bool("train", train),
		slog.Bool("trainjunk", trainJunk))

	mr := a.MessageReader(*m)
	defer func() {
		err := mr.Close()
		log.Check(err, "closing message reader after retraining")
	}()

	p, err := m.LoadPart(mr)
	if err != nil {
		log.Errorx("loading part for message", err)
		return nil
	}

	words, err := jf.ParseMessage(p)
	if err != nil {
		log.Infox("parsing message for updating junk filter", err, slog.Any("parse", ""))
		return nil
	}

	if untrain {
		err := jf.Untrain(ctx, !untrainJunk, words)
		if err != nil {
			return err
		}
		m.TrainedJunk = nil
	}
	if train {
		err := jf.Train(ctx, !trainJunk, words)
		if err != nil {
			return err
		}
		m.TrainedJunk = &trainJunk
	}
	if err := tx.Update(m); err != nil {
		return err
	}
	return nil
}

// TrainMessage trains the junk filter based on the current m.Junk/m.Notjunk flags,
// disregarding m.TrainedJunk and not updating that field.
func (a *Account) TrainMessage(ctx context.Context, log mlog.Log, jf *junk.Filter, ham bool, m Message) (bool, error) {
	mr := a.MessageReader(m)
	defer func() {
		err := mr.Close()
		log.Check(err, "closing message after training")
	}()

	p, err := m.LoadPart(mr)
	if err != nil {
		log.Errorx("loading part for message", err)
		return false, nil
	}

	words, err := jf.ParseMessage(p)
	if err != nil {
		log.Infox("parsing message for updating junk filter", err, slog.Any("parse", ""))
		return false, nil
	}

	return true, jf.Train(ctx, ham, words)
}
