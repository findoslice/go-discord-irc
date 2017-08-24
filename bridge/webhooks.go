package bridge

import (
	"encoding/json"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
)

// TODO: make longer
var webhookExpiryDuration = time.Second * 5

// A WebhookDemuxer automatically keeps track of the webhooks
// being used. If webhooks are taking too long to modify, it will
// automatically create a new one to speed up the process.
//
// It also detects removed webhooks and removes them from the system,
// guaranteeing message delivery. Messages may not be in the right order.
//
// Until the Android/Web bug is fixed, it will also handle swapping between
// two webhooks per user. It will automatically time out webhooks preserved
// for a certain user on a channel.
//
// WebhookDemuxer does not need to keep track of all the currently bridged
// channels. It only needs to know the target channel as it is being used.
//
// TODO: It does not currently delete webhooks. It may infinitely create them.
//		 Removals need to take into consideration whether all other hooks are
//		 expired, and maybe even how frequently hooks are being created.
type WebhookDemuxer struct {
	Discord  *discordBot
	webhooks []*Webhook
}

// NewWebhookDemuxer creates a new WebhookDemuxer
func NewWebhookDemuxer(bot *discordBot) *WebhookDemuxer {
	return &WebhookDemuxer{
		Discord:  bot,
		webhooks: make([]*Webhook, 0, 2),
	}
}

// Execute executes a webhook, keeping track of the username provided in WebhookParams.
func (x *WebhookDemuxer) Execute(channelID string, data *discordgo.WebhookParams) (err error) {
	// First find any existing webhooks targeting this channel
	channelWebhooks := make([]*Webhook, 0, 2)
	for _, webhook := range x.webhooks {
		if webhook.ChannelID == channelID {
			channelWebhooks = append(channelWebhooks, webhook)
		}
	}

	// The webhook to use
	var chosenWebhook *Webhook

	// Find a webhook of the same username and channel.
	// The aim here is to find the
	// - No need to check expiry here as you can't have
	//   both unexpired/expired of the same username and
	//	 channel existing at the same time.
	for _, webhook := range channelWebhooks { // searching channel webhooks
		if webhook.Username == data.Username {
			chosenWebhook = webhook
			log.Println("Found perfect webhook")
			break
		}
	}

	// If we haven't got a webhook, find an expired
	// webhook from the channel pool with any username.
	// We only care about expiry here because we don't want
	// to use a webhook that the previous user has used
	// if chosenWebhook == nil {
	// 	for _, webhook := range channelWebhooks { // searching channel webhooks
	// 		if webhook.Expired {
	// 			chosenWebhook = webhook
	// 			log.Println("Found channel-based expired webhook")
	// 			break
	// 		}
	// 	}
	// }
	// THIS DOESN'T WORK BECAUSE AN EXPIRED WEBHOOK DOES NOT IMPLY USER IS DIFFERENT

	// If we have found an expired webhook, there is the possibility that
	// there are untouched webhooks that are expired. Lets clean those up.
	if chosenWebhook != nil {
		// Android/web bug workaround:
		// lets only clean up if we have more than 2 hooks
		// This is the number stuff below:

		n := len(x.webhooks)
		// Clean up expired webhooks
		for _, webhook := range channelWebhooks {
			if n <= 2 {
				break
			}
			if webhook.Expired {
				err := x.WebhookDelete(webhook)
				if err != nil {
					log.Printf("-- Could not remove hook %s: %s", webhook.ID, err.Error())
				}
				n--
			}
		}
	}

	// So we don't have an expired webhook from our channel.
	// Lets use the oldest webhook. The most recently active from
	// our channel is going to be the most recent speaker.
	// Only necessary for the Android/web bug workaround.
	if (chosenWebhook == nil) && (len(channelWebhooks) > 1) {
		chosenWebhook = channelWebhooks[0]
		for _, webhook := range channelWebhooks {
			// So if the chosen webhook is born after (younger than)
			// the current webhook. The make the current webhook
			// our chosen webhook.
			if chosenWebhook.LastUse.After(webhook.LastUse) {
				chosenWebhook = webhook
			}
		}
	}

	// No webhook still? Take an expired one from another channel
	// and modify it to be for our channel
	if chosenWebhook == nil {
		for _, webhook := range x.webhooks { // searching global webhooks
			if (webhook.Expired) && (webhook.ChannelID != channelID) {
				log.Println("Found global unexpired webhook from another channel")
				chosenWebhook = webhook
				break
			}
		}

		if chosenWebhook != nil {
			chosenWebhook, err = chosenWebhook.ModifyChannel(x, channelID)
			if err != nil {
				// Don't panic, only log this. We have a backup scenario.
				log.Println("ERROR", err.Error())
			} else {
				log.Println("Modified aforementioned unexpired webhook to channel")
			}
		}
	}

	// If we still haven't found a webhook, create one.
	log.Println("Creating a webhook stream...")
	var newWebhook *discordgo.Webhook
	if chosenWebhook == nil {
		newWebhook, err = x.Discord.WebhookCreate(channelID, "(auto) IRC", "")
		if err != nil {
			log.Println("ERROR: Could not create webhook. Stealing expired webhook.", err)

			// We couldn't create the webhook for some reason.
			// Lets steal an expired one from somewhere...
			if len(x.webhooks) > 0 {
				chosenWebhook, err = x.webhooks[0].ModifyChannel(x, channelID)
				if err != nil {
					// Panic. We can't send our message :(
					panic(errors.Wrap(err, "Could not modify existing webhook after webhook creation failure"))
				}
				// ... if we can. But we can't. Because there aren't any webhooks to use.
			} else {
				panic(errors.Wrap(err, "No webhooks available to fall back on after webhook creation failure"))
			}
		}
	}

	// If we have created a new webhook
	if newWebhook != nil {
		log.Println("Created new webhook now, so creating wrapped webhook")
		// Create demux compatible webhook
		chosenWebhook = &Webhook{
			Webhook: newWebhook,
			// Username and Expired fields set later
		}

		// Add the newly created demux compatible webhook to our pool
		x.webhooks = append(x.webhooks, chosenWebhook)
	}

	// Ensure the webhook is not expired
	chosenWebhook.Expired = false

	// Reset the expiry ticket for the webhook
	chosenWebhook.ResetExpiry()

	// Update the webook username field
	chosenWebhook.Username = data.Username

	log.Println("--------- done, executing webhook -------")

	// TODO: What if it takes a long time? See wait=true below.
	return x.Discord.WebhookExecute(chosenWebhook.ID, chosenWebhook.Token, true, data)
}

// WebhookEdit updates an existing Webhook.
// This method is a copy of discordgo.WebhookEdit, but with added support for channelID.
// See github.com/bwmarrin/discordgo/issues/434.
//
// webhookID: The ID of a webhook.
// name     : The name of the webhook.
// avatar   : The avatar of the webhook.
func (x *WebhookDemuxer) WebhookEdit(webhookID, name, avatar, channelID string) (st *discordgo.Role, err error) {
	data := struct {
		Name      string `json:"name,omitempty"`
		Avatar    string `json:"avatar,omitempty"`
		ChannelID string `json:"channel_id,omitempty"`
	}{name, avatar, channelID}

	body, err := x.Discord.RequestWithBucketID("PATCH", discordgo.EndpointWebhook(webhookID), data, discordgo.EndpointWebhooks)
	if err != nil {
		return
	}

	// err = unmarshal(body, &st)
	// (above statement pseudo-transcluded below)
	err = json.Unmarshal(body, &st)
	if err != nil {
		return nil, discordgo.ErrJSONUnmarshal
	}

	return
}

// ContainsWebhook checks whether the pool contains the given webhookID
func (x *WebhookDemuxer) ContainsWebhook(webhookID string) (contains bool) {
	for _, webhook := range x.webhooks {
		if webhook.ID == webhookID {
			contains = true
			break
		}
	}

	return
}

// Destroy destroys the webhook demultiplexer
func (x *WebhookDemuxer) Destroy() {
	log.Println("Destroying WebhookDemuxer...")
	// Delete all the webhooks
	if len(x.webhooks) > 0 {
		// Stop all the webhooks expiry timers.
		log.Println("- Stopping hook timers...")
		for _, webhook := range x.webhooks {
			webhook.Close()
		}

		log.Println("- Removing hooks...")
		for _, webhook := range x.webhooks {
			err := x.WebhookDelete(webhook)
			if err != nil {
				log.Printf("-- Could not remove hook %s: %s", webhook.ID, err.Error())
			}
		}
		log.Println("- Hooks removed!")
	}
	log.Println("...WebhookDemuxer destroyed!")
}

// DestroyWebhook destroys the given webhook
func (x *WebhookDemuxer) WebhookDelete(w *Webhook) error {
	_, err := x.Discord.WebhookDelete(w.ID)

	// Workaround for library bug: github.com/bwmarrin/discordgo/issues/429
	if err != discordgo.ErrJSONUnmarshal {
		return err
	}
	return nil
}

// Webhook is a wrapper around discordgo.Webhook,
// tracking whether it was the last webhook that spoke in a channel.
// This struct is only necessary for the swapping functionality that
// works around the Android/Web bug.
type Webhook struct {
	*discordgo.Webhook
	Expired  bool
	Username string

	expiryTimer *time.Timer
	LastUse     time.Time
}

// ResetExpiry returns a function that resets the expiry after a certain duration
func (w *Webhook) ResetExpiry() {
	lastUse := time.Now()
	w.LastUse = lastUse

	if w.expiryTimer != nil {
		w.expiryTimer.Stop()
	}

	w.expiryTimer = time.AfterFunc(webhookExpiryDuration, func() {
		if w == nil {
			return
		}

		// This check is required because of race conditions
		// with the above w.expiryTimer.Stop() line
		if lastUse == w.LastUse {
			w.Expired = true
			log.Println("Expired webhook", w.ID)
		}
	})
}

// Close closes the webhook expiry timer
func (w *Webhook) Close() {
	if w.expiryTimer != nil {
		w.expiryTimer.Stop()
	}
}

// ModifyChannel changes the channel of a webhook
func (w *Webhook) ModifyChannel(x *WebhookDemuxer, channelID string) (*Webhook, error) {
	_, err := x.WebhookEdit(
		w.ID,
		"", "",
		channelID,
	)

	if err != nil {
		// Ah crap, so we couldn't edit the webhook to be for that channel.
		// Let's reset our chosen webhook and resort to creating a new one.
		return nil, errors.Wrap(err, "Could not modify webhook channel")
	}

	// Our library doesn't track this for us,
	// so lets update the channel ID.
	w.ChannelID = channelID
	return w, nil
}