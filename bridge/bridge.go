package bridge

import (
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// Config to be passed to New
type Config struct {
	DiscordBotToken, GuildID string

	// Map from Discord to IRC
	ChannelMappings map[string]string

	IRCServer       string
	IRCListenerName string // i.e, "DiscordBot", required to listen for messages in all cases
	WebIRCPass      string

	// InsecureSkipVerify controls whether a client verifies the
	// server's certificate chain and host name.
	// If InsecureSkipVerify is true, TLS accepts any certificate
	// presented by the server and any host name in that certificate.
	// In this mode, TLS is susceptible to man-in-the-middle attacks.
	// This should be used only for testing.
	InsecureSkipVerify bool

	// SimpleMode, when enabled, will ensure that IRCManager not spawn
	// an IRC connection for each of the online Discord users.
	SimpleMode bool

	// WebhookPrefix is prefixed to each webhook created by the Discord bot.
	WebhookPrefix string

	Suffix string // Suffix is the suffix to append to Discord users on the IRC side.

	Debug bool
}

// A Bridge represents a bridging between an IRC server and channels in a Discord server
type Bridge struct {
	Config *Config

	discord     *discordBot
	ircListener *ircListener
	ircManager  *IRCManager

	mappings []*Mapping

	done chan bool

	discordMessagesChan      chan IRCMessage
	discordMessageEventsChan chan *DiscordMessage
	updateUserChan           chan DiscordUser
}

// Close the Bridge
func (b *Bridge) Close() {
	b.done <- true
	<-b.done
}

// TODO: Use errors package
func (b *Bridge) load(opts *Config) error {
	if opts.IRCServer == "" {
		return errors.New("missing server name")
	}

	if opts.WebhookPrefix == "" {
		return errors.New("missing webhook prefix")
	}

	if err := b.SetChannelMappings(opts.ChannelMappings); err != nil {
		return errors.Wrap(err, "channel mappings could not be set")
	}

	// This should not be used anymore!
	opts.ChannelMappings = nil

	return nil
}

func (b *Bridge) SetChannelMappings(inMappings map[string]string) error {
	mappings := []*Mapping{}
	for irc, discord := range inMappings {
		mappings = append(mappings, &Mapping{
			DiscordChannel: discord,
			IRCChannel:     irc,
		})
	}

	// Check for duplicate channels
	for i, mapping := range mappings {
		for j, check := range mappings {
			if (mapping.DiscordChannel == check.DiscordChannel) || (mapping.IRCChannel == check.IRCChannel) {
				if i != j {
					return errors.New("channel_mappings contains duplicate entries")
				}
			}
		}
	}

	oldMappings := b.mappings
	b.mappings = mappings

	// If doing some changes mid-bot
	if oldMappings != nil {
		newMappings := []*Mapping{}
		removedMappings := []*Mapping{}

		// Find positive difference
		// These are the items in the new mappings list, but not the oldMappings
		for _, mapping := range mappings {
			found := false
			for _, curr := range oldMappings {
				if *curr == *mapping {
					found = true
					break
				}
			}

			if !found {
				newMappings = append(newMappings, mapping)
			}
		}

		// Find negative difference
		// These are the items in the oldMappings, but not the new one
		for _, mapping := range oldMappings {
			found := false
			for _, curr := range mappings {
				if *curr == *mapping {
					found = true
					break
				}
			}

			if !found {
				removedMappings = append(removedMappings, mapping)
			}
		}

		// The bots needs to leave the remove mappings
		rmChannels := []string{}
		for _, mapping := range removedMappings {
			// Looking for the irc channel to remove
			// inside our list of newly added channels.
			//
			// This will prevent swaps from joinquitting the bots.
			found := false
			for _, curr := range newMappings {
				if curr.IRCChannel == mapping.IRCChannel {
					found = true
				}
			}

			// If we've not found this channel to remove in the new channels
			// actually part the channel
			if !found {
				rmChannels = append(rmChannels, mapping.IRCChannel)
			}
		}

		b.ircListener.SendRaw("PART " + strings.Join(rmChannels, ","))
		for _, conn := range b.ircManager.ircConnections {
			conn.innerCon.SendRaw("PART " + strings.Join(rmChannels, ","))
		}

		// The bots needs to join the new mappings
		b.ircListener.JoinChannels()
		for _, conn := range b.ircManager.ircConnections {
			conn.JoinChannels()
		}
	}

	return nil
}

// New Bridge
func New(conf *Config) (*Bridge, error) {
	dib := &Bridge{
		Config: conf,
		done:   make(chan bool),

		discordMessagesChan:      make(chan IRCMessage),
		discordMessageEventsChan: make(chan *DiscordMessage),
		updateUserChan:           make(chan DiscordUser),
	}

	if err := dib.load(conf); err != nil {
		return nil, errors.Wrap(err, "configuration invalid")
	}

	var err error

	dib.discord, err = NewDiscord(dib, conf.DiscordBotToken, conf.GuildID)
	if err != nil {
		return nil, errors.Wrap(err, "Could not create discord bot")
	}

	dib.ircListener = NewIRCListener(dib, conf.WebIRCPass)
	dib.ircManager = NewIRCManager(dib)

	go dib.loop()

	return dib, nil
}

func (b *Bridge) SetIRCListenerName(name string) {
	b.Config.IRCListenerName = name
	b.ircListener.Nick(name)
}

func (b *Bridge) SetDebugMode(debug bool) {
	b.Config.Debug = debug
	b.ircListener.SetDebugMode(debug)

	for _, conn := range b.ircManager.ircConnections {
		conn.innerCon.Debug = debug
	}
}

// Open all the connections required to run the bridge
func (b *Bridge) Open() (err error) {

	// Open a websocket connection to Discord and begin listening.
	err = b.discord.Open()
	if err != nil {
		return errors.Wrap(err, "can't open discord")
	}

	err = b.ircListener.Connect(b.Config.IRCServer)
	if err != nil {
		return errors.Wrap(err, "can't open irc connection")
	}

	go b.ircListener.Loop()

	return
}

func rejoinIRC(con *irc.Connection, event *irc.Event) {
	if event.Arguments[1] == con.GetNick() {
		con.Join(event.Arguments[0])
	}
}

func (b *Bridge) SetupIRCConnection(con *irc.Connection, hostname, ip string) {
	con.UseTLS = true
	con.TLSConfig = &tls.Config{
		InsecureSkipVerify: b.Config.InsecureSkipVerify,
	}
	con.AddCallback("KICK", func(e *irc.Event) {
		rejoinIRC(con, e)
	})

	if b.Config.WebIRCPass != "" {
		con.WebIRC = fmt.Sprintf("%s discord %s %s", b.Config.WebIRCPass, hostname, ip)
	}
}

func (b *Bridge) GetIRCChannels() []string {
	channels := make([]string, len(b.mappings))
	for i, mapping := range b.mappings {
		channels[i] = mapping.IRCChannel
	}

	return channels
}

func (b *Bridge) GetMappingByIRC(channel string) *Mapping {
	for _, mapping := range b.mappings {
		if mapping.IRCChannel == channel {
			return mapping
		}
	}
	return nil
}

func (b *Bridge) GetMappingByDiscord(channel string) *Mapping {
	for _, mapping := range b.mappings {
		if mapping.DiscordChannel == channel {
			return mapping
		}
	}
	return nil
}

func (b *Bridge) loop() {
	for {
		select {

		// Messages from IRC to Discord
		case msg := <-b.discordMessagesChan:
			mapping := b.GetMappingByIRC(msg.IRCChannel)

			if mapping == nil {
				log.Warnln("Ignoring message sent from an unhandled IRC channel.")
				continue
			}

			avatar := b.discord.GetAvatar(b.Config.GuildID, msg.Username)
			if avatar == "" {
				// If we don't have a Discord avatar, generate an adorable avatar
				avatar = "https://api.adorable.io/avatars/128/" + msg.Username
			}

			params := discordgo.WebhookParams{
				Content:   msg.Message,
				Username:  msg.Username,
				AvatarURL: avatar,
			}

			// TODO: What if it takes a long time? See wait=true below.
			err := b.discord.whx.Execute(mapping.DiscordChannel, &params)

			if err != nil {
				log.WithFields(log.Fields{
					"error":  err,
					"params": params,
				}).Errorln("could not send message to Discord")
			}

		// Messages from Discord to IRC
		case msg := <-b.discordMessageEventsChan:
			mapping := b.GetMappingByDiscord(msg.ChannelID)

			// Do not do anything if we do not have a mapping for the channel
			if mapping == nil {
				// log.Warnln("Ignoring message sent from an unhandled Discord channel.")
				continue
			}

			b.ircManager.SendMessage(mapping.IRCChannel, msg)

		// Notification to potentially update, or create, a user
		// We should not receive anything on this channel if we're in Simple Mode
		case user := <-b.updateUserChan:
			b.ircManager.HandleUser(user)

		// Done!
		case <-b.done:
			b.discord.Close()
			b.ircListener.Quit()
			b.ircManager.Close()
			close(b.done)

			return
		}

	}
}
