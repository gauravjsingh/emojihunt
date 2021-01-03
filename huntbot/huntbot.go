package huntbot

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/gauravjsingh/emojihunt/discord"
	"github.com/gauravjsingh/emojihunt/drive"
)

type HuntBot struct {
	dis      *discord.Client
	drive    *drive.Drive
	handlers []func(*discordgo.Session, *discordgo.MessageCreate)
}

func New(dis *discord.Client, drive *drive.Drive) *HuntBot {
	return &HuntBot{dis: dis, drive: drive}
}

func (h *HuntBot) AddHandler(handler func(*discordgo.Session, *discordgo.MessageCreate)) {
	h.handlers = append(h.handlers, handler)
}

// TODO: is calling this after polling the sheet okay? every typo will turn into a sheet + channel
func (h *HuntBot) NewPuzzle(ctx context.Context, name string) error {
	id, err := h.dis.CreateChannel(name)
	if err != nil {
		return fmt.Errorf("error creating discord channel for %q: %v", name, err)
	}
	// Create Spreadsheet
	sheetURL := "https://docs.google.com/spreadsheets/d/1SgvhTBeVdyTMrCR0wZixO3O0lErh4vqX0--nBpSfYT8/edit"
	// If via bot, also take puzzle url as a param
	puzzleURL := "https://en.wikipedia.org/wiki/Main_Page"
	// Update Spreadsheet with channel URL, spreadsheet URL.

	// Post a message in the general channel with a link to the puzzle.
	if err := h.dis.GeneralChannelSend(fmt.Sprintf("There is a new puzzle %s!\nPuzzle URL: %s\nChannel <#%s>", name, puzzleURL, id)); err != nil {
		return fmt.Errorf("error posting new puzzle announcement: %v", err)
	}
	// Pin a message with the spreadsheet URL to the channel
	if err := h.dis.ChannelSendAndPin(id, fmt.Sprintf("Spreadsheet: %s\nPuzzle: %s", sheetURL, puzzleURL)); err != nil {
		return fmt.Errorf("error pinning puzzle info: %v", err)
	}
	return nil
}

func (h *HuntBot) NewPuzzleHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID || !strings.HasPrefix(m.Content, "!newpuzzle") {
		return
	}

	parts := strings.Split(m.Content, " ")
	if len(parts) < 2 {
		// send a bad usage message to the channel
		return
	}
	h.NewPuzzle(context.Background(), parts[1])
}

func (h *HuntBot) StartWork(ctx context.Context) {
	// register discord handlers to do work based on discord messages.
	//registerHandlers(h.dis)
	for _, handler := range h.handlers {
		h.dis.RegisterNewMessageHandler(handler)
	}

	// poll the sheet and trigger work based on the polling.
	// Ideally, this would only look at changes, but we start with looking at everything.
	select {
	case <-ctx.Done():
	}
}
