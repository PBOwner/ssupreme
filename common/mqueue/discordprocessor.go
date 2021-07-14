package mqueue

import (
	"encoding/json"
	"time"

	"emperror.dev/errors"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/bot"
	"github.com/jonas747/yagpdb/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

type DiscordProcessor struct {
}

func (d *DiscordProcessor) ProcessItem(resp chan *workResult, wi *workItem) {
	metricsProcessed.With(prometheus.Labels{"source": wi.elem.Source}).Inc()

	retry := false
	defer func() {
		resp <- &workResult{
			item:  wi,
			retry: retry,
		}
	}()

	queueLogger := logger.WithField("mq_id", wi.elem.ID)

	var err error
	if wi.elem.UseWebhook {
		err = trySendWebhook(queueLogger, wi.elem)
	} else {
		err = trySendNormal(queueLogger, wi.elem)
	}

	if err == nil {
		return
	}

	if e, ok := errors.Cause(err).(*discordgo.RESTError); ok {
		if (e.Response != nil && e.Response.StatusCode >= 400 && e.Response.StatusCode < 500) || (e.Message != nil && e.Message.Code != 0) {
			if source, ok := sources[wi.elem.Source]; ok {
				maybeDisableFeed(source, wi.elem, e)
			}

			return
		}
	} else {
		if onGuild, err := common.BotIsOnGuild(wi.elem.GuildID); !onGuild && err == nil {
			if source, ok := sources[wi.elem.Source]; ok {
				logger.WithError(err).Warnf("disabling feed item %s from %s to nonexistant guild", wi.elem.SourceItemID, wi.elem.Source)
				source.DisableFeed(wi.elem, err)
			}

			return
		} else if err != nil {
			logger.WithError(err).Error("failed checking if bot is on guild")
		}
	}

	if c, _ := common.DiscordError(err); c != 0 {
		return
	}

	retry = true
	queueLogger.Warn("Non-discord related error when sending message, retrying. ", err)
	time.Sleep(time.Second)

}

var disableOnError = []int{
	discordgo.ErrCodeUnknownChannel,
	discordgo.ErrCodeMissingAccess,
	discordgo.ErrCodeMissingPermissions,
	30007, // max number of webhooks
}

func maybeDisableFeed(source PluginWithSourceDisabler, elem *QueuedElement, err *discordgo.RESTError) {
	// source.HandleMQueueError(elem, errors.Cause(err))
	if err.Message == nil || !common.ContainsIntSlice(disableOnError, err.Message.Code) {
		// don't disable
		l := logger.WithError(err).WithField("source", elem.Source).WithField("sourceid", elem.SourceItemID)
		if elem.MessageEmbed != nil {
			serializedEmbed, _ := json.Marshal(elem.MessageEmbed)
			l = l.WithField("embed", serializedEmbed)
		}

		l.Error("error sending mqueue message")
		return
	}

	logger.WithError(err).Warnf("disabling feed item %s from %s", elem.SourceItemID, elem.Source)
	source.DisableFeed(elem, err)
}

func trySendNormal(l *logrus.Entry, elem *QueuedElement) (err error) {
	if elem.MessageStr != "" {
		_, err = common.BotSession.ChannelMessageSendComplex(elem.ChannelID, &discordgo.MessageSend{
			Content:         elem.MessageStr,
			AllowedMentions: elem.AllowedMentions,
		})
	} else if elem.MessageEmbed != nil {
		_, err = common.BotSession.ChannelMessageSendEmbed(elem.ChannelID, elem.MessageEmbed)
	} else {
		l.Error("Both MessageEmbed and MessageStr empty")
	}

	return
}

type cacheKeyWebhook int64

var errGuildNotFound = errors.New("Guild not found")

func trySendWebhook(l *logrus.Entry, elem *QueuedElement) (err error) {
	if elem.MessageStr == "" && elem.MessageEmbed == nil {
		l.Error("Both MessageEmbed and MessageStr empty")
		return
	}

	// find the avatar, this is slightly expensive, do i need to rethink this?
	avatar := ""
	if source, ok := sources[elem.Source]; ok {
		if avatarProvider, ok := source.(PluginWithWebhookAvatar); ok {
			avatar = avatarProvider.WebhookAvatar()
		}
	}

	gs := bot.State.Guild(true, elem.GuildID)
	if gs == nil {
		// another check just in case
		if onGuild, err := common.BotIsOnGuild(elem.GuildID); err == nil && !onGuild {
			return errGuildNotFound
		} else if err != nil {
			return err
		}
	}

	var whI interface{}
	// in some cases guild state may not be available (starting up and the like)
	if gs != nil {
		whI, err = gs.UserCacheFetch(cacheKeyWebhook(elem.ChannelID), func() (interface{}, error) {
			return findCreateWebhook(elem.GuildID, elem.ChannelID, elem.Source, avatar)
		})
	} else {
		// fallback if no gs is available
		whI, err = findCreateWebhook(elem.GuildID, elem.ChannelID, elem.Source, avatar)
		logger.WithField("guild", elem.GuildID).Warn("Guild state not available, ignoring cache")
	}

	if err != nil {
		return err
	}
	wh := whI.(*webhook)

	webhookParams := &discordgo.WebhookParams{
		Username: elem.WebhookUsername,
		Content:  elem.MessageStr,
	}

	if elem.MessageEmbed != nil {
		webhookParams.Embeds = []*discordgo.MessageEmbed{elem.MessageEmbed}
	}

	err = webhookSession.WebhookExecute(wh.ID, wh.Token, true, webhookParams)
	if code, _ := common.DiscordError(err); code == discordgo.ErrCodeUnknownWebhook {
		// if the webhook was deleted, then delete the bad boi from the databse and retry
		const query = `DELETE FROM mqueue_webhooks WHERE id=$1`
		_, err := common.PQ.Exec(query, wh.ID)
		if err != nil {
			return errors.WrapIf(err, "sql.delete")
		}

		if gs != nil {
			gs.UserCacheDel(cacheKeyWebhook(elem.ChannelID))
		}

		return errors.New("deleted webhook")
	}

	return
}
