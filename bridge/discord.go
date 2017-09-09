package bridge

import (
	"log"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
)

type discordBot struct {
	*discordgo.Session
	bridge *Bridge

	guildID string

	whx *WebhookDemuxer
}

func NewDiscord(bridge *Bridge, botToken, guildID string) (*discordBot, error) {

	// Create a new Discord session using the provided bot token.
	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, errors.Wrap(err, "discord, could not create new session")
	}
	session.StateEnabled = true

	discord := &discordBot{
		Session: session,
		bridge:  bridge,

		guildID: guildID,
	}
	discord.whx = NewWebhookDemuxer(discord)

	// These events are all fired in separate goroutines
	discord.AddHandler(discord.OnReady)
	discord.AddHandler(discord.onMessageCreate)

	if !bridge.Config.SimpleMode {
		discord.AddHandler(discord.onMemberListChunk)
		discord.AddHandler(discord.onMemberUpdate)
		discord.AddHandler(discord.OnPresencesReplace)
		discord.AddHandler(discord.OnPresenceUpdate)
	}

	return discord, nil
}

func (d *discordBot) Open() error {
	err := d.Session.Open()
	if err != nil {
		return errors.Wrap(err, "discord, could not open session")
	}

	// We need to be able to create webhooks, lets check for this.
	_, err = d.GuildWebhooks(d.bridge.Config.GuildID)
	if err != nil {
		restErr := err.(*discordgo.RESTError)
		if restErr.Message != nil && restErr.Message.Code == 50013 {
			return errors.Wrap(err, "The bot does not have the 'Manage Webhooks' permission.")
		}

		panic(err)
	}

	return nil
}

func (d *discordBot) Close() error {
	d.whx.Destroy()
	return d.Session.Close()
}

func (d *discordBot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore all messages created by the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Ignore messages sent from our webhooks
	if d.whx.ContainsWebhook(m.Author.ID) {
		return
	}

	// If the message is "ping" reply with "Pong!"
	if m.Content == "ping" {
		s.ChannelMessageSend(m.ChannelID, "Pong!")
	}

	content := d.ParseText(m.Message)

	// Special Mee6 behaviour
	if m.Author.ID == "159985870458322944" {
		content = strings.Replace(
			content,
			`CompSoc is The University of Edinburgh's society for anyone interested in Informatics.  This server is linked up with IRC, another way of chatting, so you'll find there are a lot more people listening than what it shows on Discord.`,
			"",
			-1,
		)
	}

	// The content is an action if it matches "_(.+)_"
	isAction := len(content) > 2 &&
		m.Content[0] == '_' &&
		m.Content[len(content)-1] == '_'

	// If it is an action, remove the enclosing underscores
	if isAction {
		content = content[1 : len(m.Content)-1]
	}

	d.bridge.discordMessageEventsChan <- &DiscordMessage{
		Message:  m.Message,
		Content:  content,
		IsAction: isAction,
	}

	for _, attachment := range m.Attachments {
		d.bridge.discordMessageEventsChan <- &DiscordMessage{
			Message:  m.Message,
			Content:  attachment.URL,
			IsAction: isAction,
		}
	}
}

// Up to date as of https://git.io/v5kJg
var channelMention = regexp.MustCompile(`<#(\d+)>`)
var roleMention = regexp.MustCompile(`<@&(\d+)>`)

// Up to date as of https://git.io/v5kJg
func (d *discordBot) ParseText(m *discordgo.Message) string {
	// Content with @user mentions replaced
	content, err := m.ContentWithMoreMentionsReplaced(d.Session)
	if err != nil {
		log.Println("Error getting content with mentions replaced")
		return m.ContentWithMentionsReplaced()
	}

	// Sanitise multiple lines in a single message
	content = strings.Replace(content, "\r\n", "\n", -1) // replace CRLF with LF
	content = strings.Replace(content, "\r", "\n", -1)   // replace CR with LF
	content = strings.Replace(content, "\n", " ", -1)    // replace LF with " "

	// Replace <#xxxxx> channel mentions
	content = channelMention.ReplaceAllStringFunc(content, func(str string) string {
		// Strip enclosing identifiers
		channelID := str[2 : len(str)-1]

		channel, err := d.State.Channel(channelID)
		if err == nil {
			return "#" + channel.Name
		} else if err == discordgo.ErrStateNotFound {
			return "#deleted-channel"
		}

		panic(errors.Wrap(err, "Channel mention failed for "+str))
	})

	// Replace <@&xxxxx> role mentions
	content = roleMention.ReplaceAllStringFunc(content, func(str string) string {
		// Strip enclosing identifiers
		roleID := str[3 : len(str)-1]

		role, err := d.State.Role(d.bridge.Config.GuildID, roleID)
		if err == nil {
			return "@" + role.Name
		} else if err == discordgo.ErrStateNotFound {
			return "@deleted-role"
		}

		panic(errors.Wrap(err, "Channel mention failed for "+str))
	})

	return content
}

func (d *discordBot) onMemberListChunk(s *discordgo.Session, m *discordgo.GuildMembersChunk) {
	for _, m := range m.Members {
		d.handleMemberUpdate(m)
	}
}

func (d *discordBot) onMemberUpdate(s *discordgo.Session, m *discordgo.GuildMemberUpdate) {
	d.handleMemberUpdate(m.Member)
}

// What does this do? Probably what it sounds like.
func (d *discordBot) OnPresencesReplace(s *discordgo.Session, m *discordgo.PresencesReplace) {
	for _, p := range *m {
		d.handlePresenceUpdate(p)
	}
}

// Handle when presence is updated
func (d *discordBot) OnPresenceUpdate(s *discordgo.Session, m *discordgo.PresenceUpdate) {
	d.handlePresenceUpdate(&m.Presence)
}

func (d *discordBot) handlePresenceUpdate(p *discordgo.Presence) {
	// If they are offline, just deliver a mostly empty struct with the ID and online state
	if p.Status == "offline" {
		d.bridge.updateUserChan <- DiscordUser{
			ID:     p.User.ID,
			Online: false,
		}
		return
	}

	// Otherwise get their GuildMember object...
	user, err := d.State.Member(d.guildID, p.User.ID)
	if err != nil {
		log.Println(err.Error())
		return
	}

	// .. and handle as per usual
	d.handleMemberUpdate(user)
}

func (d *discordBot) OnReady(s *discordgo.Session, m *discordgo.Ready) {
	d.RequestGuildMembers(d.guildID, "", 0)
}

func (d *discordBot) handleMemberUpdate(m *discordgo.Member) {
	// This error is usually triggered on first run because it represents offline
	presence, err := d.State.Presence(d.guildID, m.User.ID)
	if err != nil {
		// TODO: Determine the type of the error, and handle non-offline situations
		return
	}

	if presence.Status == "offline" {
		return
	}

	d.bridge.updateUserChan <- DiscordUser{
		ID:            m.User.ID,
		Discriminator: m.User.Discriminator,
		Nick:          GetMemberNick(m),
		Bot:           m.User.Bot,
		Online:        presence.Status != "offline",
	}
}

// See https://github.com/reactiflux/discord-irc/pull/230/files#diff-7202bb7fb017faefd425a2af32df2f9dR357
func (d *discordBot) GetAvatar(guildID, username string) (_ string) {
	// First get all members
	guild, err := d.State.Guild(guildID)
	if err != nil {
		panic(err)
	}

	// Matching members
	var foundMember *discordgo.Member

	// First check an exact match, aborting on multiple
	for _, member := range guild.Members {
		if (username != member.Nick) && (username != member.User.Username) {
			continue
		}

		if foundMember == nil {
			foundMember = member
		} else {
			return
		}
	}

	// If no member found, check case-insensitively
	if foundMember == nil {
		for _, member := range guild.Members {
			if !strings.EqualFold(username, member.Nick) && !strings.EqualFold(username, member.User.Username) {
				continue
			}

			if foundMember == nil {
				foundMember = member
			} else {
				return
			}
		}
	}

	// Do not provide an avatar if:
	// - no matching user OR
	// - multiple matching users
	if foundMember == nil {
		return
	}

	return discordgo.EndpointUserAvatar(foundMember.User.ID, foundMember.User.Avatar)
}

// GetMemberNick returns the real display name for a Discord GuildMember
func GetMemberNick(m *discordgo.Member) string {
	if m.Nick == "" {
		return m.User.Username
	}

	return m.Nick
}
