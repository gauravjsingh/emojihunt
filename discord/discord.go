package discord

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

type Config struct {
	QMChannelName, GeneralChannelName, TechChannelName string
	PuzzleCategoryName, SolvedCategoryName             string
	QMRoleName                                         string
}

type Client struct {
	s       *discordgo.Session
	guildID string
	// The QM channel contains a central log of interesting bot actions, as well as the only place for
	// advanced bot usage, such as puzzle or round creation.
	qmChannelID string
	// The general channel has all users, and has announcements from the bot.
	generalChannelID string
	// The tech channel has error messages.
	techChannelID string
	// The puzzle channel category.
	puzzleCategoryID string
	// The category for solved puzzles.
	solvedCategoryID string
	// The Role ID for the QM role.
	qmRoleID string

	// This might be a case where sync.Map makes sense.
	mu              sync.Mutex
	channelNameToID map[string]string
}

func getGuildID(s *discordgo.Session) (string, error) {
	gs := s.State.Guilds
	if len(gs) != 1 {
		return "", fmt.Errorf("expected exactly 1 guild, found %d", len(gs))
	}
	return gs[0].ID, nil
}

func New(s *discordgo.Session, c Config) (*Client, error) {
	guildID, err := getGuildID(s)
	if err != nil {
		return nil, err
	}
	chs, err := s.GuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("error creating channel ID cache: %v", err)
	}
	chIDs := make(map[string]string)
	for _, ch := range chs {
		chIDs[ch.Name] = ch.ID
	}
	qm, ok := chIDs[c.QMChannelName]
	if !ok {
		return nil, fmt.Errorf("QM Channel %q not found", c.QMChannelName)
	}
	puz, ok := chIDs[c.PuzzleCategoryName]
	if !ok {
		return nil, fmt.Errorf("puzzle category %q not found", c.PuzzleCategoryName)
	}
	ar, ok := chIDs[c.SolvedCategoryName]
	if !ok {
		return nil, fmt.Errorf("archive %q not found", c.SolvedCategoryName)
	}
	gen, ok := chIDs[c.GeneralChannelName]
	if !ok {
		gen = qm
	}
	tech, ok := chIDs[c.TechChannelName]
	if !ok {
		tech = qm
	}
	roles, err := s.GuildRoles(guildID)
	if err != nil {
		return nil, fmt.Errorf("error fetching roles: %v", err)
	}
	var qmRoleID string
	for _, r := range roles {
		if r.Name == c.QMRoleName {
			qmRoleID = r.ID
			break
		}
	}
	if qmRoleID == "" {
		return nil, fmt.Errorf("QM role %q not found in roles: %v", c.QMRoleName, roles)
	}

	return &Client{
		s:                s,
		guildID:          guildID,
		qmChannelID:      qm,
		generalChannelID: gen,
		techChannelID:    tech,
		channelNameToID:  chIDs,
		puzzleCategoryID: puz,
		solvedCategoryID: ar,
		qmRoleID:         qmRoleID,
	}, nil
}

// TODO: Make this a struct with a name.
type NewMessageHandler func(*discordgo.Session, *discordgo.MessageCreate) error

func (c *Client) RegisterNewMessageHandler(name string, h NewMessageHandler) {
	// Only handle new guild messages.
	// TODO: bitOr with the current value.
	c.s.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsGuildMessages)
	c.s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if err := h(s, m); err != nil {
			log.Printf("%s: %v", name, err)
		}
	})
}

// TODO: id or name?
func (c *Client) ChannelURL(id string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s", c.guildID, id)
}

func (c *Client) ChannelID(url string) (string, error) {
	if !strings.HasPrefix(url, "https://discord.com/channels/"+c.guildID+"/") {
		return "", fmt.Errorf("invalid channel URL: %q", url)
	}
	parts := strings.Split(url, "/")
	return parts[len(parts)-1], nil
}

func (c *Client) ChannelSend(chanID, msg string) error {
	_, err := c.s.ChannelMessageSend(chanID, msg)
	return err
}

func (c *Client) ChannelSendAndPin(chanID, msg string) error {
	m, err := c.s.ChannelMessageSend(chanID, msg)
	if err != nil {
		return err
	}
	err = c.s.ChannelMessagePin(chanID, m.ID)
	return err
}

const statusPrefix = "**=== Puzzle Information ===**"

// Set the pinned status message, by posting one or editing the existing one.
// No-op if the status was already set.
func (c *Client) SetPinnedInfo(chanID, spreadsheetURL, puzzleURL, status string) (didUpdate bool, err error) {
	ms, err := c.s.ChannelMessagesPinned(chanID)
	if err != nil {
		return false, err
	}
	var statusMessage *discordgo.Message
	for _, m := range ms {
		if strings.HasPrefix(m.Content, statusPrefix) {
			if statusMessage != nil {
				log.Printf("Multiple status messages in %v, editing last one", chanID)
			}
			statusMessage = m
		}
	}

	msg := fmt.Sprintf("%s\nSpreadsheet: <%s>\nPuzzle: <%s>",
		statusPrefix, spreadsheetURL, puzzleURL)
	if status != "" {
		msg = fmt.Sprintf("%s\nStatus: %s", msg, status)
	}

	if statusMessage == nil {
		err := c.ChannelSendAndPin(chanID, msg)
		return err == nil, err
	} else if statusMessage.Content == msg {
		return false, nil // no-op
	}

	_, err = c.s.ChannelMessageEdit(chanID, statusMessage.ID, msg)
	return err == nil, err
}

func (c *Client) QMChannelSend(msg string) error {
	_, err := c.s.ChannelMessageSend(c.qmChannelID, msg)
	return err
}

func (c *Client) GeneralChannelSend(msg string) error {
	_, err := c.s.ChannelMessageSend(c.generalChannelID, msg)
	return err
}

func (c *Client) GeneralChannelSendEmbed(embed *discordgo.MessageEmbed) error {
	_, err := c.s.ChannelMessageSendEmbed(c.generalChannelID, embed)
	return err
}

func (c *Client) TechChannelSend(msg string) error {
	_, err := c.s.ChannelMessageSend(c.techChannelID, msg)
	return err
}

var ChannelNotFound = fmt.Errorf("channel not found")

// Returns whether a channel was archived.
func (c *Client) ArchiveChannel(chID string) (bool, error) {
	ch, err := c.s.Channel(chID)
	if err != nil {
		return false, fmt.Errorf("channel id %s not found: %w", chID, ChannelNotFound)
	}

	// Already archived.
	if ch.ParentID == c.solvedCategoryID {
		return false, nil
	}

	arCh, err := c.s.Channel(c.solvedCategoryID)
	if err != nil {
		return false, fmt.Errorf("error looking up archive: %v", err)
	}

	_, err = c.s.ChannelEditComplex(chID, &discordgo.ChannelEdit{ParentID: c.solvedCategoryID, PermissionOverwrites: arCh.PermissionOverwrites})
	if err != nil {
		return false, fmt.Errorf("error moving channel: %v", err)
	}
	return true, nil
}

// CreateChannel ensures that a channel exists with the given name, and returns the channel ID.
func (c *Client) CreateChannel(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, ok := c.channelNameToID[name]; ok {
		return id, nil
	}
	ch, err := c.s.GuildChannelCreateComplex(c.guildID, discordgo.GuildChannelCreateData{
		Name:     name,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: c.puzzleCategoryID,
	})
	if err != nil {
		return "", fmt.Errorf("error creating channel %q: %v", name, err)
	}
	c.channelNameToID[name] = ch.ID
	return ch.ID, nil
}

func (c *Client) QMHandler(s *discordgo.Session, m *discordgo.MessageCreate) error {
	if m.Author.ID == s.State.User.ID || !strings.HasPrefix(m.Content, "!qm") {
		return nil
	}

	parts := strings.Split(m.Content, " ")
	if len(parts) < 2 {
		// send a bad usage message to the channel
		return fmt.Errorf("not able to find qm command from %q", m.Content)
	}

	switch parts[1] {
	case "start":
		s.GuildMemberRoleAdd(c.guildID, m.Author.ID, c.qmRoleID)
	case "stop":
		s.GuildMemberRoleRemove(c.guildID, m.Author.ID, c.qmRoleID)
	default:
		return fmt.Errorf("unexpected QM command: %q", parts[1])
	}
	return nil
}
