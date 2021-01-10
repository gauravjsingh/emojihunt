package huntbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gauravjsingh/emojihunt/discord"
	"github.com/gauravjsingh/emojihunt/drive"
)

type HuntBot struct {
	dis   *discord.Client
	drive *drive.Drive

	mu           sync.Mutex              // hold while accessing everything below
	enabled      bool                    // global killswitch, toggle with !huntbot kill/!huntbot start
	puzzleStatus map[string]drive.Status // name -> status (best-effort cache)
	archived     map[string]bool         // name -> channel was archived (best-effort cache)
}

func New(dis *discord.Client, d *drive.Drive) *HuntBot {
	return &HuntBot{
		dis:          dis,
		drive:        d,
		enabled:      true,
		puzzleStatus: map[string]drive.Status{},
		archived:     map[string]bool{},
	}
}

func (h *HuntBot) notifyNewPuzzle(puzzle *drive.PuzzleInfo, channelID string) error {
	log.Printf("Posting information about new puzzle %q", puzzle.Name)
	// TODO: also edit sheet to link to channel/puzzle

	// Pin a message with the spreadsheet URL to the channel
	if _, err := h.dis.SetPinnedInfo(channelID, puzzle.DocURL, puzzle.PuzzleURL, ""); err != nil {
		return fmt.Errorf("error pinning puzzle info: %v", err)
	}

	// Post a message in the general channel with a link to the puzzle.
	embed := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{Name: "A new puzzle is available!"},
		Title:  puzzle.Name,
		URL:    puzzle.PuzzleURL,
		Color:  0xd9e9d4,
		Fields: []*discordgo.MessageEmbedField{
			&discordgo.MessageEmbedField{
				Name:   "Round",
				Value:  fmt.Sprintf("%s %s", puzzle.Round.Emoji, puzzle.Round.Name),
				Inline: false,
			},
			&discordgo.MessageEmbedField{
				Name:   "Channel",
				Value:  fmt.Sprintf("<#%s>", channelID),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Puzzle",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.PuzzleURL),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Sheet",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.DocURL),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Status",
				Value:  fmt.Sprintf("%s", puzzle.Status),
				Inline: true,
			},
		},
	}
	if err := h.dis.GeneralChannelSendEmbed(embed); err != nil {
		return fmt.Errorf("error posting new puzzle announcement: %v", err)
	}

	return nil
}

func (h *HuntBot) NewPuzzle(ctx context.Context, name string) error {
	_, err := h.dis.CreateChannel(name)
	if err != nil {
		return fmt.Errorf("error creating discord channel for %q: %v", name, err)
	}
	// Create Spreadsheet
	_, err = h.drive.CreateSheet(ctx, name, "Unknown Round") // TODO
	if err != nil {
		return fmt.Errorf("error creating spreadsheet for %q: %v", name, err)
	}

	// If via bot, also take puzzle url as a param
	//	puzzleURL := "https://en.wikipedia.org/wiki/Main_Page"

	return fmt.Errorf("Temporarily disabling !newpuzzle functionality")
}

func (h *HuntBot) NewPuzzleHandler(s *discordgo.Session, m *discordgo.MessageCreate) error {
	if m.Author.ID == s.State.User.ID || !strings.HasPrefix(m.Content, "!newpuzzle") {
		return nil
	}

	parts := strings.Split(m.Content, " ")
	if len(parts) < 2 {
		// send a bad usage message to the channel
		return fmt.Errorf("not able to find new puzzle name from %q", m.Content)
	}
	if err := h.NewPuzzle(context.Background(), parts[1]); err != nil {
		return fmt.Errorf("error creating puzzle: %v", err)
	}
	return nil
}

func (h *HuntBot) setPuzzleStatus(name string, newStatus drive.Status) (oldStatus drive.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	oldStatus = h.puzzleStatus[name]
	h.puzzleStatus[name] = newStatus
	return oldStatus
}

// logStatus marks the status; it is *not* called if the puzzle is solved
func (h *HuntBot) logStatus(ctx context.Context, puzzle *drive.PuzzleInfo) error {
	channelID, err := h.dis.ChannelID(puzzle.DiscordURL)
	if err != nil {
		return err
	}

	didUpdate, err := h.dis.SetPinnedInfo(channelID, puzzle.DocURL, puzzle.DiscordURL, string(puzzle.Status))
	if err != nil {
		return fmt.Errorf("unable to set puzzle status message for %q: %w", puzzle.Name, err)
	}

	if didUpdate {
		if err := h.dis.GeneralChannelSend(fmt.Sprintf("Puzzle %q is now %v.", puzzle.Name, puzzle.Status)); err != nil {
			return fmt.Errorf("error posting puzzle status announcement: %v", err)
		}
	}

	return nil
}

func (h *HuntBot) markSolved(ctx context.Context, puzzle *drive.PuzzleInfo) error {
	channelID, err := h.dis.ChannelID(puzzle.DiscordURL)
	if err != nil {
		return err
	}

	if puzzle.Answer == "" {
		if err := h.dis.ChannelSend(channelID, fmt.Sprintf("Puzzle solved!  Please add the answer to the sheet.")); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}

		if err := h.dis.QMChannelSend(fmt.Sprintf("Puzzle %q marked solved, but has no answer, please add it to the sheet.", puzzle.Name)); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}

		return nil // don't archive until we have the answer.
	}

	archived, err := h.dis.ArchiveChannel(channelID)
	if !archived {
		// Channel already archived (cache is best-effort -- this can happen
		// after restart or if a human did it)
	} else if err != nil {
		return fmt.Errorf("unable to archive channel for %q: %v", puzzle.Name, err)
	} else {
		log.Printf("Archiving channel for %q", puzzle.Name)
		// post to relevant channels only if it was newly archived.
		if err := h.dis.ChannelSend(channelID, fmt.Sprintf("Puzzle solved! The answer was %v. I'll archive this channel.", puzzle.Answer)); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}

		if err := h.dis.GeneralChannelSend(fmt.Sprintf("Puzzle %q was solved!", puzzle.Name)); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}
	}

	log.Printf("Marking sheet solved for %q", puzzle.Name)
	err = h.drive.MarkSheetSolved(ctx, puzzle.DocURL)
	if err != nil {
		return err
	}

	h.archive(puzzle.Name)

	return nil
}

func (h *HuntBot) isArchived(puzzleName string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.archived[puzzleName]
}

func (h *HuntBot) archive(puzzleName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.archived[puzzleName] = true
}

func (h *HuntBot) updatePuzzle(ctx context.Context, puzzle *drive.PuzzleInfo) error {
	// TODO: warn if puzzle.Name is set but others haven't been for a while?
	if puzzle.Name == "" || puzzle.PuzzleURL == "" || puzzle.Round.Name == "" {
		return nil
	}

	var err error
	if puzzle.DocURL == "" {
		puzzle.DocURL, err = h.drive.CreateSheet(ctx, puzzle.Name, puzzle.Round.Name)
		if err != nil {
			return fmt.Errorf("error creating spreadsheet for %q: %v", puzzle.Name, err)
		}
	}

	if puzzle.DiscordURL == "" {
		log.Printf("Adding channel for new puzzle %q", puzzle.Name)
		id, err := h.dis.CreateChannel(puzzle.Name)
		if err != nil {
			return fmt.Errorf("error creating discord channel for %q: %v", puzzle.Name, err)
		}

		puzzle.DiscordURL = h.dis.ChannelURL(id)

		// Treat discord URL as the sentinel to also notify everyone
		if err := h.notifyNewPuzzle(puzzle, id); err != nil {
			return fmt.Errorf("error notifying channel about new puzzle %q: %v", puzzle.Name, err)
		}
		if err := h.drive.SetDiscordURL(ctx, puzzle); err != nil {
			return fmt.Errorf("error setting discord URL for puzzle %q: %v", puzzle.Name, err)
		}
	}

	if h.setPuzzleStatus(puzzle.Name, puzzle.Status) != puzzle.Status ||
		puzzle.Answer != "" && puzzle.Status.IsSolved() && !h.isArchived(puzzle.Name) {
		// (potential) status change
		if puzzle.Status.IsSolved() {
			if err := h.markSolved(ctx, puzzle); err != nil {
				return fmt.Errorf("failed to mark puzzle %q solved: %v", puzzle.Name, err)
			}
		} else {
			if err := h.logStatus(ctx, puzzle); err != nil {
				return fmt.Errorf("failed to mark puzzle %q %v: %v", puzzle.Name, puzzle.Status, err)
			}
		}
	}

	return nil
}

func (h *HuntBot) pollAndUpdate(ctx context.Context) error {
	puzzles, err := h.drive.ReadFullSheet()
	if err != nil {
		return err
	}

	for _, puzzle := range puzzles {
		err := h.updatePuzzle(ctx, puzzle)
		if err != nil {
			// log, but proceed to the next puzzle.
			log.Printf("updating puzzle failed: %v", err)
		}
	}

	if err := h.drive.UpdateAllURLs(ctx, puzzles); err != nil {
		return fmt.Errorf("error updating URLs for puzzles: %v", err)
	}

	return nil
}

func (h *HuntBot) WatchSheet(ctx context.Context) {
	// we don't have a way to subscribe to updates, so we just poll the sheet
	// TODO: if sheet last-mod is since our last run, noop
	failures := 0
	for {
		if h.isEnabled() {
			err := h.pollAndUpdate(ctx)
			if err != nil {
				// log always, but ping after 3 consecutive failures, then every 10, to avoid spam
				log.Printf("watching sheet failed: %v", err)
				failures++
				if failures%10 == 3 {
					h.dis.TechChannelSend(fmt.Sprintf("watching sheet failed: %v", err))
				}
			} else {
				failures = 0
			}
		} else {
			log.Printf("bot disabled, skipping update")
		}

		select {
		case <-ctx.Done():
			log.Print("exiting watcher due to signal")
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (h *HuntBot) isEnabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled
}

func (h *HuntBot) ControlHandler(s *discordgo.Session, m *discordgo.MessageCreate) error {
	if m.Author.ID == s.State.User.ID || !strings.HasPrefix(m.Content, "!huntbot") {
		return nil
	}

	h.mu.Lock()

	reply := ""
	info := ""
	switch m.Content {
	case "!huntbot kill":
		if h.enabled {
			h.enabled = false
			reply = `Ok, I've disabled the bot for now.  Enable it with "!huntbot start".`
			info = fmt.Sprintf("**bot disabled by %v**", m.Author.Mention())
		} else {
			reply = `The bot was already disabled.  Enable it with "!huntbot start".`
		}
	case "!huntbot start":
		if !h.enabled {
			h.enabled = true
			reply = `Ok, I've enabled the bot for now.  Disable it with "!huntbot kill".`
			info = fmt.Sprintf("**bot enabled by %v**", m.Author.Mention())
		} else {
			reply = `The bot was already enabled.  Disable it with "!huntbot kill".`
		}
	default:
		reply = `I'm not sure what you mean.  Disable the bot with "!huntbot kill" ` +
			`or enable it with "!huntbot start".`
	}

	h.mu.Unlock()

	s.ChannelMessageSend(m.ChannelID, reply)
	if info != "" {
		h.dis.TechChannelSend(info)
		log.Printf(info)
	}

	return nil
}
