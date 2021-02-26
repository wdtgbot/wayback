// Copyright 2020 Wayback Archiver. All rights reserved.
// Use of this source code is governed by the GNU GPL v3
// license that can be found in the LICENSE file.

package telegram // import "github.com/wabarc/wayback/service/telegram"

import (
	"context"
	"sync"

	"github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/wabarc/helper"
	"github.com/wabarc/wayback"
	"github.com/wabarc/wayback/config"
	"github.com/wabarc/wayback/errors"
	"github.com/wabarc/wayback/logger"
	"github.com/wabarc/wayback/publish"
)

type telegram struct {
	opts *config.Options

	bot *tgbotapi.BotAPI
	upd tgbotapi.Update
}

// New telegram struct.
func New(opts *config.Options) *telegram {
	return &telegram{
		opts: opts,
	}
}

// Serve loop request message from the Telegram api server.
// Serve always returns a nil error.
func (t *telegram) Serve(ctx context.Context) (err error) {
	if t.bot, err = tgbotapi.NewBotAPI(t.opts.TelegramToken()); err != nil {
		return errors.New("Initialize telegram failed, error: %v", err)
	}

	logger.Info("Telegram: authorized on account %s", t.bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := t.bot.GetUpdatesChan(u)
	if err != nil {
		return errors.New("Get telegram message channel failed, error: %v", err)
	}

	for update := range updates {
		if update.Message == nil { // ignore any non-Message Updates
			continue
		}

		t.upd = update
		go t.process(ctx)
	}

	return nil
}

func (t *telegram) process(ctx context.Context) {
	bot, update := t.bot, t.upd
	message := update.Message
	text := message.Text
	logger.Debug("Telegram: message: %s", text)

	urls := helper.MatchURL(text)
	switch {
	case message.IsCommand():
		return
	case len(urls) == 0:
		logger.Info("Telegram: archives failure, URL no found.")
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "URL no found.")
		msg.ReplyToMessageID = update.Message.MessageID
		bot.Send(msg)
		return
	}

	col, err := t.archive(urls)
	if err != nil {
		logger.Error("Telegram: archives failure, ", err)
		return
	}

	replyText := publish.Render(col)
	logger.Debug("Telegram: reply text, %s", replyText)
	msg := tgbotapi.NewMessage(update.Message.Chat.ID, replyText)
	msg.ReplyToMessageID = update.Message.MessageID
	msg.ParseMode = "html"

	bot.Send(msg)

	if t.opts.PublishToChannel() {
		logger.Debug("Telegram: publishing to channel...")
		publish.ToChannel(t.opts, bot, replyText)
	}
	if t.opts.PublishToIssues() {
		logger.Debug("Telegram: publishing to GitHub issues...")
		publish.ToIssues(ctx, t.opts, publish.NewGitHub().Render(col))
	}
	if t.opts.PublishToMastodon() {
		mstdn := publish.NewMastodon(nil, t.opts)
		mstdn.ToMastodon(ctx, t.opts, mstdn.Render(col), "")
	}
}

func (t *telegram) archive(urls []string) (col []*wayback.Collect, err error) {
	logger.Debug("Telegram: archives start...")

	wg := sync.WaitGroup{}
	var wbrc wayback.Broker = &wayback.Handle{URLs: urls, Opts: t.opts}
	for slot, arc := range t.opts.Slots() {
		if !arc {
			continue
		}
		wg.Add(1)
		go func(slot string) {
			defer wg.Done()
			c := &wayback.Collect{}
			logger.Debug("Telegram: archiving slot: %s", slot)
			switch slot {
			case config.SLOT_IA:
				c.Dst = wbrc.IA()
			case config.SLOT_IS:
				c.Dst = wbrc.IS()
			case config.SLOT_IP:
				c.Dst = wbrc.IP()
			case config.SLOT_PH:
				c.Dst = wbrc.PH()
			}
			c.Arc = config.SlotName(slot)
			c.Ext = config.SlotExtra(slot)
			col = append(col, c)
		}(slot)
	}
	wg.Wait()

	return col, nil
}
