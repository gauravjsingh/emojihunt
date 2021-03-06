package huntbot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gauravjsingh/emojihunt/discord"
	"github.com/gauravjsingh/emojihunt/drive"
)

type Config struct {
	// How often to warn in discord about badly formatted puzzles.
	MinWarningFrequency time.Duration
	InitialWarningDelay time.Duration
	UpdateRooms         bool
}

type HuntBot struct {
	dis   *discord.Client
	drive *drive.Drive
	cfg   Config

	mu           sync.Mutex              // hold while accessing everything below
	enabled      bool                    // global killswitch, toggle with !huntbot kill/!huntbot start
	puzzleStatus map[string]drive.Status // name -> status (best-effort cache)
	archived     map[string]bool         // name -> channel was archived (best-effort cache)
	// When we last warned about a malformed puzzle.
	lastWarnTime map[string]time.Time
}

func New(dis *discord.Client, d *drive.Drive, c Config) *HuntBot {
	return &HuntBot{
		dis:          dis,
		drive:        d,
		enabled:      true,
		puzzleStatus: map[string]drive.Status{},
		archived:     map[string]bool{},
		lastWarnTime: map[string]time.Time{},
		cfg:          c,
	}
}

const pinnedStatusHeader = "Puzzle Information"

func (h *HuntBot) setPinnedStatusInfo(puzzle *drive.PuzzleInfo, channelID string) (didUpdate bool, err error) {
	embed := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{Name: pinnedStatusHeader},
		Color:  puzzle.Round.IntColor(),
		Title:  puzzle.Name,
		URL:    puzzle.PuzzleURL,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Round",
				Value:  fmt.Sprintf("%v %v", puzzle.Round.Emoji, puzzle.Round.Name),
				Inline: false,
			},
			{
				Name:   "Status",
				Value:  puzzle.Status.Pretty(),
				Inline: true,
			},
			{
				Name:   "Puzzle",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.PuzzleURL),
				Inline: true,
			},
			{
				Name:   "Sheet",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.DocURL),
				Inline: true,
			},
		},
	}

	return h.dis.CreateUpdatePin(channelID, pinnedStatusHeader, embed)
}

const roomStatusHeader = "Working Room"

func (h *HuntBot) setPinnedVoiceInfo(puzzleChannelID string, voiceChannelID *string) (didUpdate bool, err error) {
	room := "No voice room set. \"!room start $room\" to start working in $room."
	if voiceChannelID != nil {
		room = fmt.Sprintf("Join us in <#%s>!", *voiceChannelID)
	}
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: roomStatusHeader},
		Description: room,
	}

	return h.dis.CreateUpdatePin(puzzleChannelID, roomStatusHeader, embed)
}

func (h *HuntBot) notifyNewPuzzle(puzzle *drive.PuzzleInfo, channelID string) error {
	log.Printf("Posting information about new puzzle %q", puzzle.Name)

	// Pin a message with the spreadsheet URL to the channel
	if _, err := h.setPinnedStatusInfo(puzzle, channelID); err != nil {
		return fmt.Errorf("error pinning puzzle info: %v", err)
	}

	// Post a message in the general channel with a link to the puzzle.
	embed := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{
			Name:    "A new puzzle is available!",
			IconURL: puzzle.Round.TwemojiURL(),
		},
		Color: puzzle.Round.IntColor(),
		Title: puzzle.Name,
		URL:   puzzle.PuzzleURL,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Channel",
				Value:  fmt.Sprintf("<#%s>", channelID),
				Inline: true,
			},
			{
				Name:   "Puzzle",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.PuzzleURL),
				Inline: true,
			},
			{
				Name:   "Sheet",
				Value:  fmt.Sprintf("[Link](%s)", puzzle.DocURL),
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
	id, err := h.dis.CreateChannel(name)
	if err != nil {
		return fmt.Errorf("error creating discord channel for %q: %v", name, err)
	}
	// Create Spreadsheet
	sheetURL, err := h.drive.CreateSheet(ctx, name, "Unknown Round") // TODO
	if err != nil {
		return fmt.Errorf("error creating spreadsheet for %q: %v", name, err)
	}

	// If via bot, also take puzzle url as a param
	puzzleURL := "https://en.wikipedia.org/wiki/Main_Page"

	puzzleInfo := &drive.PuzzleInfo{
		Name:      name,
		Round:     drive.Round{Emoji: "", Name: "(Unknown)"},
		PuzzleURL: puzzleURL,
		DocURL:    sheetURL,
	}
	return h.notifyNewPuzzle(puzzleInfo, id)
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

	didUpdate, err := h.setPinnedStatusInfo(puzzle, channelID)
	if err != nil {
		return fmt.Errorf("unable to set puzzle status message for %q: %w", puzzle.Name, err)
	}

	if didUpdate {
		if err := h.dis.StatusUpdateChannelSend(fmt.Sprintf("%s Puzzle <#%s> is now %v.", puzzle.Round.Emoji, channelID, puzzle.Status.Pretty())); err != nil {
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

	verb := "solved"
	if puzzle.Status == drive.Backsolved {
		verb = "backsolved"
	}

	if puzzle.Answer == "" {
		if err := h.dis.ChannelSend(channelID, fmt.Sprintf("Puzzle %s!  Please add the answer to the sheet.", verb)); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}

		if err := h.dis.QMChannelSend(fmt.Sprintf("Puzzle %q marked %s, but has no answer, please add it to the sheet.", puzzle.Name, verb)); err != nil {
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
		if err := h.dis.ChannelSend(channelID, fmt.Sprintf("Puzzle %s! The answer was `%v`. I'll archive this channel.", verb, puzzle.Answer)); err != nil {
			return fmt.Errorf("error posting solved puzzle announcement: %v", err)
		}

		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
				Name:    fmt.Sprintf("Puzzle %s!", verb),
				IconURL: puzzle.Round.TwemojiURL(),
			},
			Color: puzzle.Round.IntColor(),
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "Channel",
					Value:  fmt.Sprintf("<#%s>", channelID),
					Inline: true,
				},
				{
					Name:   "Answer",
					Value:  fmt.Sprintf("`%s`", puzzle.Answer),
					Inline: true,
				},
			},
		}

		if err := h.dis.GeneralChannelSendEmbed(embed); err != nil {
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

func (h *HuntBot) warnPuzzle(ctx context.Context, puzzle *drive.PuzzleInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lastWarning, ok := h.lastWarnTime[puzzle.Name]; !ok {
		h.lastWarnTime[puzzle.Name] = time.Now().Add(h.cfg.InitialWarningDelay - h.cfg.MinWarningFrequency)
	} else if time.Now().Sub(lastWarning) <= h.cfg.MinWarningFrequency {
		return nil
	}
	var msgs []string
	if puzzle.PuzzleURL == "" {
		msgs = append(msgs, "missing a URL")
	}
	if puzzle.Round.Name == "" {
		msgs = append(msgs, "missing a round")
	}
	if len(msgs) == 0 {
		return fmt.Errorf("cannot warn about well-formatted puzzle %q: %v", puzzle.Name, puzzle)
	}
	if err := h.dis.QMChannelSend(fmt.Sprintf("Puzzle %q is %s", puzzle.Name, strings.Join(msgs, " and "))); err != nil {
		return err
	}
	h.lastWarnTime[puzzle.Name] = time.Now()
	return nil
}

func (h *HuntBot) updatePuzzle(ctx context.Context, puzzle *drive.PuzzleInfo) error {
	if puzzle.Name == "" || puzzle.PuzzleURL == "" || puzzle.Round.Name == "" {
		// Occasionally warn the QM about puzzles that are missing fields.
		if puzzle.Name != "" {
			if err := h.warnPuzzle(ctx, puzzle); err != nil {
				return fmt.Errorf("error warning about malformed puzzle %q: %v", puzzle.Name, err)
			}
		}
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
	puzzles, err := h.drive.ReadFullSheet(ctx)
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

var roomRE = regexp.MustCompile(`!room (start|stop)(?: (.*))?$`)

func (h *HuntBot) RoomHandler(s *discordgo.Session, m *discordgo.MessageCreate) error {
	if m.Author.ID == s.State.User.ID || !strings.HasPrefix(m.Content, "!room") {
		return nil
	}

	// TODO: reply errors are not caught.
	var reply string
	defer func(reply *string) {
		if *reply == "" {
			return
		}
		s.ChannelMessageSend(m.ChannelID, *reply)
	}(&reply)

	matches := roomRE.FindStringSubmatch(m.Content)
	if len(matches) != 3 {
		// Not a command
		reply = fmt.Sprintf("Invalid command %q. Voice command must be of the form \"!room start $room\" or \"!room stop $room\" where $room is a voice channel", m.Content)
		return nil
	}

	puzzle, ok := h.drive.PuzzleForChannelURL(h.dis.ChannelURL(m.ChannelID))
	if !ok {
		reply = fmt.Sprintf("Unable to get puzzle name for channel ID %q. Contact @tech.", m.ChannelID)
		return fmt.Errorf("unable to get puzzle name for channel ID %q", m.ChannelID)
	}

	var rID string
	if matches[2] != "" {
		rID, ok = h.dis.ClosestRoomID(matches[2])
		if !ok {
			reply = fmt.Sprintf("Unable to find room %q. Available rooms are: %v", matches[2], strings.Join(h.dis.AvailableRooms(), ", "))
			return nil
		}
	}

	// Note that discord only allows updating a channel name twice per 10 minutes, so this will often take 10+ minutes.
	switch matches[1] {
	case "start":
		if rID == "" {
			reply = "!room start requires a room"
			return fmt.Errorf("missing room ID from command: %s", m.Content)
		}
		if h.cfg.UpdateRooms {
			if rID == "" {
				reply = "!room start requires a room"
				return fmt.Errorf("missing room ID from command: %s", m.Content)
			}
			updated, err := h.dis.AddPuzzleToRoom(puzzle, rID)
			if err != nil {
				reply = "error updating room name, contact @tech."
				return err
			}
			if !updated {
				reply = fmt.Sprintf("Puzzle %q is already in room %s", puzzle, discord.ChannelMention(rID))
				return nil
			}
		}
		h.setPinnedVoiceInfo(m.ChannelID, &rID)
		reply = fmt.Sprintf("Set the room for puzzle %q to %s", puzzle, discord.ChannelMention(rID))
	case "stop":
		if h.cfg.UpdateRooms {
			if rID == "" {
				reply = "!room stop requires a room to update room names"
				return fmt.Errorf("missing room ID from command: %s", m.Content)
			}
			updated, err := h.dis.RemovePuzzleFromRoom(puzzle, rID)
			if err != nil {
				reply = "error updating room name, contact @tech."
				return err
			}
			if !updated {
				reply = fmt.Sprintf("Puzzle %q was already not in room %s", puzzle, discord.ChannelMention(rID))
				return nil
			}
		}
		h.setPinnedVoiceInfo(m.ChannelID, nil)
		reply = fmt.Sprintf("Removed the room for puzzle %q", puzzle)
	default:
		reply = fmt.Sprintf("Unrecognized voice bot action %q. Valid commands are \"!room start $RoomName\" or \"!room start $RoomName\"", m.Content)
		return fmt.Errorf("impossible voice bot action %q: %q", matches[1], m.Content)
	}

	return nil
}
