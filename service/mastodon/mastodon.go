// Copyright 2021 Wayback Archiver. All rights reserved.
// Use of this source code is governed by the GNU GPL v3
// license that can be found in the LICENSE file.

package mastodon // import "github.com/wabarc/wayback/service/mastodon"

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-mastodon"
	"github.com/wabarc/helper"
	"github.com/wabarc/wayback"
	"github.com/wabarc/wayback/config"
	"github.com/wabarc/wayback/errors"
	"github.com/wabarc/wayback/logger"
	"github.com/wabarc/wayback/publish"
	"golang.org/x/net/html"
)

type Mastodon struct {
	sync.RWMutex

	opts *config.Options

	client *mastodon.Client
	status *mastodon.Status
	convID mastodon.ID

	archiving map[mastodon.ID]bool
}

// New mastodon struct.
func New(opts *config.Options) *Mastodon {
	if !opts.PublishToMastodon() {
		logger.Fatal("Missing required environment variable")
	}

	client := mastodon.NewClient(&mastodon.Config{
		Server:       opts.MastodonServer(),
		ClientID:     opts.MastodonClientKey(),
		ClientSecret: opts.MastodonClientSecret(),
		AccessToken:  opts.MastodonAccessToken(),
	})
	return &Mastodon{
		opts:   opts,
		client: client,
	}
}

// Serve loop request direct messages from the Mastodon instance.
// Serve always returns a nil error.
func (m *Mastodon) Serve(ctx context.Context) error {
	logger.Debug("[mastodon] Serving Mastodon instance: %s", m.opts.MastodonServer())

	// rcv, err := m.client.StreamingUser(ctx)
	// if err != nil {
	// 	logger.Error("%v", err)
	// 	return err
	// }
	// for e := range rcv {
	// 	switch t := e.(type) {
	// 	case *mastodon.UpdateEvent:
	// 		logger.Debug("%v", t.Status)

	// 		m.status = t.Status
	// 		go m.process(ctx)
	// 	case *mastodon.ErrorEvent:
	// 		logger.Error("%v", e)
	// 	}
	// }

	// Clear notifications every 10 minutes
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			m.client.ClearNotifications(ctx)
		}
	}()

	mutex := sync.RWMutex{}
	m.archiving = make(map[mastodon.ID]bool)
	for {
		convs, err := m.client.GetConversations(ctx, nil)
		if err != nil {
			logger.Error("[mastodon] Get conversations failure, error: %v", err)
		}
		logger.Debug("[mastodon] conversations: %v", convs)

		for _, conv := range convs {
			m.status = conv.LastStatus
			m.convID = conv.ID
			if _, exist := m.archiving[m.convID]; exist {
				continue
			}
			go m.process(ctx)

			mutex.Lock()
			m.archiving[m.convID] = true
			mutex.Unlock()
		}
		time.Sleep(5 * time.Second)
	}
}

func (m *Mastodon) process(ctx context.Context) error {
	if m.status == nil || m.convID == "" {
		logger.Debug("[mastodon] no status or conversation")
		return errors.New("Mastodon: no status or conversation")
	}

	text := textContent(m.status.Content)
	logger.Debug("[mastodon] conversation id: %s message: %s", m.convID, text)
	defer m.client.DeleteConversation(ctx, m.convID)
	defer func() {
		time.Sleep(time.Second)
		delete(m.archiving, m.convID)
	}()

	urls := helper.MatchURL(text)
	pub := publish.NewMastodon(m.client, m.opts)
	if len(urls) == 0 {
		logger.Info("[mastodon] archives failure, URL no found.")
		pub.ToMastodon(ctx, m.opts, "URL no found", string(m.status.ID))
		return errors.New("Mastodon: URL no found")
	}

	col, err := m.archive(urls)
	if err != nil {
		logger.Error("[mastodon] archives failure, ", err)
		return err
	}

	replyText := pub.Render(col)
	logger.Debug("[mastodon] reply text, %s", replyText)
	pub.ToMastodon(ctx, m.opts, replyText, string(m.status.ID))

	if m.opts.PublishToChannel() {
		logger.Debug("[mastodon] publishing to Telegram channel...")
		publish.ToChannel(m.opts, nil, replyText)
	}
	if m.opts.PublishToIssues() {
		logger.Debug("[mastodon] publishing to GitHub issues...")
		publish.ToIssues(ctx, m.opts, publish.NewGitHub().Render(col))
	}
	if m.opts.PublishToTwitter() {
		logger.Debug("[mastodon] publishing to Twitter...")
		twitter := publish.NewTwitter(nil, m.opts)
		twitter.ToTwitter(ctx, m.opts, twitter.Render(col))
	}

	return nil
}

func (m *Mastodon) archive(urls []string) (col []*wayback.Collect, err error) {
	logger.Debug("[mastodon] archives start...")

	wg := sync.WaitGroup{}
	var wbrc wayback.Broker = &wayback.Handle{URLs: urls, Opts: m.opts}
	for slot, arc := range m.opts.Slots() {
		if !arc {
			continue
		}
		wg.Add(1)
		go func(slot string) {
			defer wg.Done()
			c := &wayback.Collect{}
			logger.Debug("[mastodon] archiving slot: %s", slot)
			switch slot {
			case config.SLOT_IA:
				c.Arc = config.SlotName(slot)
				c.Dst = wbrc.IA()
			case config.SLOT_IS:
				c.Arc = config.SlotName(slot)
				c.Dst = wbrc.IS()
			case config.SLOT_IP:
				c.Arc = config.SlotName(slot)
				c.Dst = wbrc.IP()
			case config.SLOT_PH:
				c.Arc = config.SlotName(slot)
				c.Dst = wbrc.PH()
			}
			col = append(col, c)
		}(slot)
	}
	wg.Wait()

	return col, nil
}

func textContent(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var buf bytes.Buffer

	var extractText func(node *html.Node, w *bytes.Buffer)
	extractText = func(node *html.Node, w *bytes.Buffer) {
		if node.Type == html.TextNode {
			data := strings.Trim(node.Data, "\r\n")
			if data != "" {
				w.WriteString(data)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			extractText(c, w)
		}
		if node.Type == html.ElementNode {
			name := strings.ToLower(node.Data)
			if name == "br" {
				w.WriteString("\n")
			}
		}
	}
	extractText(doc, &buf)
	return buf.String()
}