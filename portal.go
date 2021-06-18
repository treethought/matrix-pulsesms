// matrix-pulsesms - A Matrix-PulseSMS puppeting bridge.
// Copyright (C) 2021 Cam Sweeney
// Copyright (C) 2020 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"html"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/treethought/matrix-pulsesms/database"
	"github.com/treethought/pulsesms"
)

func (bridge *Bridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByMXID[mxid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByMXID(mxid), nil)
	}
	return portal
}

func (bridge *Bridge) GetPortalByPID(key database.PortalKey) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByPID[key]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByPID(key), &key)
	}
	return portal
}

func (bridge *Bridge) GetAllPortals() []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAll())
}

func (bridge *Bridge) GetAllPortalsByPID(pid pulsesms.PhoneNumber) []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAllByPID(pid))
}

func (bridge *Bridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}
		portal, ok := bridge.portalsByPID[dbPortal.Key]
		if !ok {
			portal = bridge.loadDBPortal(dbPortal, nil)
		}
		output[index] = portal
	}
	return output
}

func (bridge *Bridge) loadDBPortal(dbPortal *database.Portal, key *database.PortalKey) *Portal {
	if dbPortal == nil {
		if key == nil {
			return nil
		}
		dbPortal = bridge.DB.Portal.New()
		dbPortal.Key = *key
		dbPortal.Insert()
	}
	portal := bridge.NewPortal(dbPortal)
	bridge.portalsByPID[portal.Key] = portal
	if len(portal.MXID) > 0 {
		bridge.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

func (portal *Portal) GetUsers() []*User {
	return nil
}

func (bridge *Bridge) NewManualPortal(key database.PortalKey) *Portal {
	portal := &Portal{
		Portal: bridge.DB.Portal.New(),
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Portal/%s", key)),

		recentlyHandled: [recentlyHandledLength]pulsesms.MessageID{},

		messages: make(chan PortalMessage, bridge.Config.Bridge.PortalMessageBuffer),
	}
	portal.Key = key
	go portal.handleMessageLoop()
	return portal
}

func (bridge *Bridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.Key)),

		recentlyHandled: [recentlyHandledLength]pulsesms.MessageID{},

		messages: make(chan PortalMessage, bridge.Config.Bridge.PortalMessageBuffer),
	}
	go portal.handleMessageLoop()
	return portal
}

const recentlyHandledLength = 100

type PortalMessage struct {
	chat      string
	source    *User
	data      interface{}
	timestamp uint64
}

type Portal struct {
	*database.Portal

	bridge *Bridge
	log    log.Logger

	roomCreateLock sync.Mutex
	encryptLock    sync.Mutex

	recentlyHandled      [recentlyHandledLength]pulsesms.MessageID
	recentlyHandledLock  sync.Mutex
	recentlyHandledIndex uint8

	backfillLock  sync.Mutex
	backfilling   bool
	lastMessageTs uint64

	privateChatBackfillInvitePuppet func()

	messages chan PortalMessage

	isPrivate   *bool
	isBroadcast *bool
	hasRelaybot *bool
}

const MaxMessageAgeToCreatePortal = 5 * 60 // 5 minutes

func (portal *Portal) syncDoublePuppetDetailsAfterCreate(source *User) {
	doublePuppet := portal.bridge.GetPuppetByCustomMXID(source.MXID)
	if doublePuppet == nil {
		return
	}

	// TODO move to pulsesms repo
	ok := false
	var chat pulsesms.Chat
	for _, c := range source.Pulse.Store.Chats {
		if c.ID == portal.Key.PID {
			chat = c
			ok = true
			break
		}
	}
	if !ok {
		portal.log.Debugln("Not syncing chat mute/tags with %s: chat info not found", source.MXID)
		return
	}

	source.syncChatDoublePuppetDetails(doublePuppet, Chat{
		Chat:   chat,
		Portal: portal,
	}, true)
}

func (portal *Portal) handleMessageLoop() {
	for msg := range portal.messages {
		if len(portal.MXID) == 0 {
			if msg.timestamp+MaxMessageAgeToCreatePortal < uint64(time.Now().Unix()) {
				portal.log.Debugln("Not creating portal room for incoming message: message is too old")
				continue
			} else if !portal.shouldCreateRoom(msg) {
				portal.log.Debugln("Not creating portal room for incoming message: message is not a chat message")
				continue
			}
			portal.log.Debugln("Creating Matrix room from incoming message")
			err := portal.CreateMatrixRoom(msg.source)
			if err != nil {
				portal.log.Errorln("Failed to create portal room:", err)
				return
			}
			portal.syncDoublePuppetDetailsAfterCreate(msg.source)
		}
		portal.backfillLock.Lock()
		portal.handleMessage(msg, false)
		portal.backfillLock.Unlock()
	}
}

func (portal *Portal) shouldCreateRoom(msg PortalMessage) bool {
	return true
}

func (portal *Portal) handleMessage(msg PortalMessage, isBackfill bool) {
	portal.log.Debugfln("handling portal message")
	if len(portal.MXID) == 0 {
		portal.log.Warnln("handleMessage called even though portal.MXID is empty")
		return
	}

	// TODO: handle media messages
	switch data := msg.data.(type) {
	case pulsesms.Message:
		portal.HandleTextMessage(msg.source, data)
	default:
		portal.log.Warnln("Unknown message type:", reflect.TypeOf(msg.data))
	}
}

func (portal *Portal) isRecentlyHandled(id pulsesms.MessageID) bool {
	start := portal.recentlyHandledIndex
	for i := start; i != start; i = (i - 1) % recentlyHandledLength {
		if portal.recentlyHandled[i] == id {
			return true
		}
	}
	return false
}

func (portal *Portal) isDuplicate(id pulsesms.MessageID) bool {
	msg := portal.bridge.DB.Message.GetByPID(portal.Key, fmt.Sprint(id))
	if msg != nil {
		return true
	}
	return false
}

func (portal *Portal) markHandled(source *User, message *pulsesms.Message, mxid id.EventID, isSent bool) *database.Message {
	msg := portal.bridge.DB.Message.New()
	msg.Chat = portal.Key
	msg.PID = message.ID
	msg.MXID = mxid
	msg.Timestamp = int64(message.Timestamp)

	if message.From != "" {
		msg.Sender = message.From
	} else if message.SentDevice > 0 {
		msg.Sender = source.PID
	}

	msg.Sender = message.From
	if len(msg.Sender) == 0 {
		msg.Sender = portal.Key.PID
	}
	msg.Content = message.Data
	msg.Sent = isSent
	msg.Insert()

	portal.recentlyHandledLock.Lock()
	index := portal.recentlyHandledIndex
	portal.recentlyHandledIndex = (portal.recentlyHandledIndex + 1) % recentlyHandledLength
	portal.recentlyHandledLock.Unlock()
	portal.recentlyHandled[index] = msg.PID
	return msg
}

func (portal *Portal) getMessageIntent(user *User, info pulsesms.Message) *appservice.IntentAPI {

	// TODO: make this cleaner once https://github.com/Serubin/pulse-sms-web/issues/161 is figured out

	fromPID := info.From
	portal.log.Debugfln("getting intent for message sender: %s", fromPID)
	portal.log.Debugfln("message %+v\n", info)

	// from was set, just lookup
	if info.From != "" {
		contact, ok := user.Pulse.GetContactByName(fromPID)
		if ok {
			puppet := portal.bridge.GetPuppetByPID(contact.PhoneNumber)
			puppet.SyncContactIfNecessary(user)
			return puppet.IntentFor(portal)
		}
		contact, ok = user.Pulse.GetContactByPhone(fromPID)
		if ok {
			puppet := portal.bridge.GetPuppetByPID(contact.PhoneNumber)
			puppet.SyncContactIfNecessary(user)
			return puppet.IntentFor(portal)
		}
	}

	// TODO show as user mxid, need to shend message if sent from phone
	// sent from bridge user, don't resend message into portal room
	if info.SentDevice > 0 {
		portal.log.Infofln("message was sent by bridge user")
        return nil
	}

	// message received from contact

	// if only one member, its from them
	chat, ok := user.Pulse.GetChat(portal.Key.PID)
	if !ok {
		portal.log.Warnfln("could not find chat for pulse message, using main intent")
		return portal.MainIntent()
	}

	if len(chat.Members) == 0 {
		portal.log.Warnfln("Chat %s has no members, returning main intent", chat.ID)
		return portal.MainIntent()

	}
	// log.Warnfln("GOT CHAT FOR MISSING FROM")
	// log.Warnfln("%+v", chat)

	if len(chat.Members) == 1 {
		if chat.Members[0] == "" {
			return portal.MainIntent()
		}
		portal.log.Infofln("one member, using member phone: %s", chat.Members[0])
		puppet := portal.bridge.GetPuppetByPID(chat.Members[0])
		puppet.SyncContactIfNecessary(user)
		return puppet.IntentFor(portal)
	} else {
		portal.log.Warnfln("Unknown sender in group chat, using main intent")
		return portal.MainIntent()
	}
}

func (portal *Portal) startHandling(source *User, info pulsesms.Message) *appservice.IntentAPI {
	// TODO these should all be trace logs
	// if portal.lastMessageTs == 0 {
	// 	portal.log.Debugln("Fetching last message from database to get its timestamp")
	// 	lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.Key)
	// 	if lastMessage != nil {
	// 		atomic.CompareAndSwapUint64(&portal.lastMessageTs, 0, uint64(lastMessage.Timestamp))
	// 	}
	// }
	if int64(portal.lastMessageTs) > info.Timestamp+1 {
		portal.log.Infofln("Not handling %d: message is older (%d) than last bridge message (%d)", info.ID, info.Timestamp, portal.lastMessageTs)
	} else if portal.isRecentlyHandled(info.ID) {
		portal.log.Infofln("Not handling %s: message was recently handled", info.ID)
	} else if portal.isDuplicate(info.ID) {
		portal.log.Debugfln("Not handling %s: message is duplicate", info.ID)
	} else {
		portal.lastMessageTs = uint64(info.Timestamp)
		intent := portal.getMessageIntent(source, info)
		if intent != nil {
			portal.log.Debugfln("Starting handling of %d (ts: %d)", info.ID, info.Timestamp)
		} else {
			portal.log.Debugfln("Not handling %s: sender is not known", info.ID)
		}
		return intent
	}
	return nil
}

func (portal *Portal) finishHandling(source *User, message pulsesms.Message, mxid id.EventID) {
	portal.markHandled(source, &message, mxid, true)
	portal.sendDeliveryReceipt(mxid)
	portal.log.Debugln("Handled message", message.ID, "->", mxid)
}

func (portal *Portal) kickExtraUsers(participantMap map[pulsesms.PhoneNumber]bool) {
	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Warnln("Failed to get member list:", err)
	} else {
		for member := range members.Joined {
			pid, ok := portal.bridge.ParsePuppetMXID(member)
			if ok {
				_, shouldBePresent := participantMap[pid]
				if !shouldBePresent {
					_, err = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{
						UserID: member,
						Reason: "User had left this PulseSMS chat",
					})
					if err != nil {
						portal.log.Warnfln("Failed to kick user %s who had left: %v", member, err)
					}
				}
			}
		}
	}
}

func (portal *Portal) SyncBroadcastRecipients(source *User, members []pulsesms.PhoneNumber) {
	participantMap := make(map[pulsesms.PhoneNumber]bool)
	for _, recipient := range members {
		participantMap[recipient] = true

		puppet := portal.bridge.GetPuppetByPID(recipient)
		puppet.SyncContactIfNecessary(source)
		err := puppet.DefaultIntent().EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to make puppet of %s join %s: %v", recipient, portal.MXID, err)
		}
	}
	portal.kickExtraUsers(participantMap)
}

func (portal *Portal) SyncParticipants(source *User, members []pulsesms.PhoneNumber) {
	changed := false
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
		changed = true
	}
	participantMap := make(map[pulsesms.PhoneNumber]bool)
	for _, participant := range members {
		participantMap[participant] = true
		user := portal.bridge.GetUserByPID(participant)

		portal.userMXIDAction(user, portal.ensureMXIDInvited)

		puppet := portal.bridge.GetPuppetByPID(participant)

		puppet.SyncContactIfNecessary(source)
		err = puppet.IntentFor(portal).EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to make puppet of %s join %s: %v", participant, portal.MXID, err)
		}

		expectedLevel := 0
		// if participant.IsSuperAdmin {
		// 	expectedLevel = 95
		// } else if participant.IsAdmin {
		// 	expectedLevel = 50
		// }
		changed = levels.EnsureUserLevel(puppet.MXID, expectedLevel) || changed
		if user != nil {
			changed = levels.EnsureUserLevel(user.MXID, expectedLevel) || changed
		}
	}
	if changed {
		_, err = portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		}
	}
	portal.kickExtraUsers(participantMap)
}

type profilePicInfo struct {
	URL    string
	Tag    string
	status string
}

func (portal *Portal) UpdateAvatar(user *User, avatar *profilePicInfo, updateInfo bool) bool {
	return false
	// if avatar == nil || (avatar.Status == 0 && avatar.Tag != "remove" && len(avatar.URL) == 0) {
	// 	var err error
	// 	avatar, err = user.Conn.GetProfilePicThumb(portal.Key.PID)
	// 	if err != nil {
	// 		portal.log.Errorln(err)
	// 		return false
	// 	}
	// }

	// if avatar.Status == 404 {
	// 	avatar.Tag = "remove"
	// 	avatar.Status = 0
	// }
	// if avatar.Status != 0 || portal.Avatar == avatar.Tag {
	// 	return false
	// }

	// if avatar.Tag == "remove" {
	// 	portal.AvatarURL = id.ContentURI{}
	// } else {
	// 	data, err := avatar.DownloadBytes()
	// 	if err != nil {
	// 		portal.log.Warnln("Failed to download avatar:", err)
	// 		return false
	// 	}

	// 	mimeType := http.DetectContentType(data)
	// 	resp, err := portal.MainIntent().UploadBytes(data, mimeType)
	// 	if err != nil {
	// 		portal.log.Warnln("Failed to upload avatar:", err)
	// 		return false
	// 	}

	// 	portal.AvatarURL = resp.ContentURI
	// }

	// if len(portal.MXID) > 0 {
	// 	_, err := portal.MainIntent().SetRoomAvatar(portal.MXID, portal.AvatarURL)
	// 	if err != nil {
	// 		portal.log.Warnln("Failed to set room topic:", err)
	// 		return false
	// 	}
	// }
	// portal.Avatar = avatar.Tag
	// if updateInfo {
	// 	portal.UpdateBridgeInfo()
	// }
	// return true
}

func (portal *Portal) UpdateName(name string, setBy pulsesms.PhoneNumber, intent *appservice.IntentAPI, updateInfo bool) bool {
	if portal.Name != name {
		portal.log.Debugfln("Updating name %s -> %s", portal.Name, name)
		portal.Name = name
		if intent == nil {
			intent = portal.MainIntent()
			if len(setBy) > 0 {
				intent = portal.bridge.GetPuppetByPID(setBy).IntentFor(portal)
			}
		}
		_, err := intent.SetRoomName(portal.MXID, name)
		if err == nil {
			if updateInfo {
				portal.UpdateBridgeInfo()
			}
			return true
		} else {
			portal.Name = ""
			portal.log.Warnln("Failed to set room name:", err)
		}
	}
	return false
}

func (portal *Portal) UpdateTopic(topic string, setBy pulsesms.PhoneNumber, intent *appservice.IntentAPI, updateInfo bool) bool {
	if portal.Topic != topic {
		portal.log.Debugfln("Updating topic %s -> %s", portal.Topic, topic)
		portal.Topic = topic
		if intent == nil {
			intent = portal.MainIntent()
			if len(setBy) > 0 {
				intent = portal.bridge.GetPuppetByPID(setBy).IntentFor(portal)
			}
		}
		_, err := intent.SetRoomTopic(portal.MXID, topic)
		if err == nil {
			if updateInfo {
				portal.UpdateBridgeInfo()
			}
			return true
		} else {
			portal.Topic = ""
			portal.log.Warnln("Failed to set room topic:", err)
		}
	}
	return false
}

func (portal *Portal) UpdateMetadata(user *User) bool {
	if portal.IsPrivateChat() {
		return false
	}

	// TODO get from db?
	chat, ok := user.Pulse.Store.Chats[portal.Key.PID]
	if !ok {
		portal.log.Errorfln("couldnt find chat %s while updating metadata", portal.Key.PID)
		return false
	}

	members := chat.Members

	portal.SyncParticipants(user, members)
	update := false
	update = portal.UpdateName(chat.Name, "", nil, false) || update
	// update = portal.UpdateTopic(metadata.Topic, metadata.TopicSetBy, nil, false) || update

	portal.RestrictMessageSending(false)

	return update
}

func (portal *Portal) userMXIDAction(user *User, fn func(mxid id.UserID)) {
	if user == nil {
		return
	}

	if user == portal.bridge.Relaybot {
		for _, mxid := range portal.bridge.Config.Bridge.Relaybot.InviteUsers {
			fn(mxid)
		}
	} else {
		fn(user.MXID)
	}
}

func (portal *Portal) ensureMXIDInvited(mxid id.UserID) {
	err := portal.MainIntent().EnsureInvited(portal.MXID, mxid)
	if err != nil {
		portal.log.Warnfln("Failed to ensure %s is invited to %s: %v", mxid, portal.MXID, err)
	}
}

func (portal *Portal) ensureUserInvited(user *User) {
	portal.userMXIDAction(user, portal.ensureMXIDInvited)

	customPuppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
	if customPuppet != nil && customPuppet.CustomIntent() != nil {
		_ = customPuppet.CustomIntent().EnsureJoined(portal.MXID)
	}
}

func (portal *Portal) Sync(user *User, chat pulsesms.Chat) {
	portal.log.Infofln("Syncing portal for user: %s, chat: %s", user.MXID, chat.Name)

	if user.IsRelaybot {
		yes := true
		portal.hasRelaybot = &yes
	}

	if len(portal.MXID) == 0 {
		if !portal.IsPrivateChat() {
			portal.Name = chat.Name
		}
		err := portal.CreateMatrixRoom(user)
		if err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
			return
		}
	} else {
		portal.ensureUserInvited(user)
	}

	update := false
	update = portal.UpdateMetadata(user) || update
	if !portal.IsPrivateChat() && portal.Avatar == "" {
		update = portal.UpdateAvatar(user, nil, false) || update
	}
	if update {
		portal.Update()
		portal.UpdateBridgeInfo()
	}
}

func (portal *Portal) GetBasePowerLevels() *event.PowerLevelsEventContent {
	anyone := 0
	nope := 99
	invite := 50
	if portal.bridge.Config.Bridge.AllowUserInvite {
		invite = 0
	}
	return &event.PowerLevelsEventContent{
		UsersDefault:    anyone,
		EventsDefault:   anyone,
		RedactPtr:       &anyone,
		StateDefaultPtr: &nope,
		BanPtr:          &nope,
		InvitePtr:       &invite,
		Users: map[id.UserID]int{
			portal.MainIntent().UserID: 100,
		},
		Events: map[string]int{
			event.StateRoomName.Type:   anyone,
			event.StateRoomAvatar.Type: anyone,
			event.StateTopic.Type:      anyone,
		},
	}
}

func (portal *Portal) ChangeAdminStatus(pids []string, setAdmin bool) id.EventID {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}
	newLevel := 0
	if setAdmin {
		newLevel = 50
	}
	changed := false
	for _, pid := range pids {
		puppet := portal.bridge.GetPuppetByPID(pid)
		changed = levels.EnsureUserLevel(puppet.MXID, newLevel) || changed

		user := portal.bridge.GetUserByPID(pid)
		if user != nil {
			changed = levels.EnsureUserLevel(user.MXID, newLevel) || changed
		}
	}
	if changed {
		resp, err := portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		} else {
			return resp.EventID
		}
	}
	return ""
}

func (portal *Portal) RestrictMessageSending(restrict bool) id.EventID {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}

	newLevel := 0
	if restrict {
		newLevel = 50
	}

	if levels.EventsDefault == newLevel {
		return ""
	}

	levels.EventsDefault = newLevel
	resp, err := portal.MainIntent().SetPowerLevels(portal.MXID, levels)
	if err != nil {
		portal.log.Errorln("Failed to change power levels:", err)
		return ""
	} else {
		return resp.EventID
	}
}

func (portal *Portal) RestrictMetadataChanges(restrict bool) id.EventID {
	levels, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		levels = portal.GetBasePowerLevels()
	}
	newLevel := 0
	if restrict {
		newLevel = 50
	}
	changed := false
	changed = levels.EnsureEventLevel(event.StateRoomName, newLevel) || changed
	changed = levels.EnsureEventLevel(event.StateRoomAvatar, newLevel) || changed
	changed = levels.EnsureEventLevel(event.StateTopic, newLevel) || changed
	if changed {
		resp, err := portal.MainIntent().SetPowerLevels(portal.MXID, levels)
		if err != nil {
			portal.log.Errorln("Failed to change power levels:", err)
		} else {
			return resp.EventID
		}
	}
	return ""
}

func (portal *Portal) BackfillHistory(user *User, lastMessageTime int64) error {
	if !portal.bridge.Config.Bridge.RecoverHistory {
		return nil
	}

	endBackfill := portal.beginBackfill()
	defer endBackfill()

	// TODO: lastmessage keeps being nil, not being set right
	// lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.Key)
	// if lastMessage == nil {
	// 	portal.log.Warnfln("lastmessage nil, stopping")
	// 	return nil
	// }
	// if lastMessage.Timestamp >= lastMessageTime {
	// 	portal.log.Warnln("Not backfilling: no new messages")
	// 	return nil
	// }
	chat, ok := user.Pulse.GetChat(portal.Key.PID)
	if !ok {
		portal.log.Errorfln("failed to get chat by portal pid")
		return nil
	}

	chatId, err := strconv.Atoi(chat.ID)
	if err != nil {
		portal.log.Errorfln("invalid chat id")
		return nil
	}

	msgs, err := user.Pulse.GetMessages(chatId, 0)
	if err != nil {
		portal.log.Errorfln("failed to fetch messages")
		return nil
	}
	portal.handleHistory(user, msgs)

	// lastMessageID := fmt.Sprint(lastMessage.PID)
	// lastMessageFromMe := lastMessage.Sender == user.PID
	// for len(lastMessageID) > 0 {
	// 	portal.log.Debugln("Fetching 50 messages of history after", lastMessageID)

	// 	portal.handleHistory(user, msgs)

	// 	lastMessageProto, ok := messages[len(messages)-1].(*waProto.WebMessageInfo)
	// 	if ok {
	// 		lastMessageID = lastMessageProto.GetKey().GetId()
	// 		lastMessageFromMe = lastMessageProto.GetKey().GetFromMe()
	// 	}
	// }
	portal.log.Infoln("Backfilling finished")
	return nil
}

func (portal *Portal) beginBackfill() func() {
	portal.backfillLock.Lock()
	portal.backfilling = true
	var privateChatPuppetInvited bool
	var privateChatPuppet *Puppet
	if portal.IsPrivateChat() && portal.bridge.Config.Bridge.InviteOwnPuppetForBackfilling && portal.Key.PID != portal.Key.Receiver {
		privateChatPuppet = portal.bridge.GetPuppetByPID(portal.Key.Receiver)
		portal.privateChatBackfillInvitePuppet = func() {
			if privateChatPuppetInvited {
				return
			}
			privateChatPuppetInvited = true
			_, _ = portal.MainIntent().InviteUser(portal.MXID, &mautrix.ReqInviteUser{UserID: privateChatPuppet.MXID})
			_ = privateChatPuppet.DefaultIntent().EnsureJoined(portal.MXID)
		}
	}
	return func() {
		portal.backfilling = false
		portal.privateChatBackfillInvitePuppet = nil
		portal.backfillLock.Unlock()
		if privateChatPuppet != nil && privateChatPuppetInvited {
			_, _ = privateChatPuppet.DefaultIntent().LeaveRoom(portal.MXID)
		}
	}
}

func (portal *Portal) disableNotifications(user *User) {
	if !portal.bridge.Config.Bridge.HistoryDisableNotifs {
		return
	}
	puppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
	if puppet == nil || puppet.customIntent == nil {
		return
	}
	portal.log.Debugfln("Disabling notifications for %s for backfilling", user.MXID)
	ruleID := fmt.Sprintf("dev.spherics.silence_while_backfilling.%s", portal.MXID)
	err := puppet.customIntent.PutPushRule("global", pushrules.OverrideRule, ruleID, &mautrix.ReqPutPushRule{
		Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		Conditions: []pushrules.PushCondition{{
			Kind:    pushrules.KindEventMatch,
			Key:     "room_id",
			Pattern: string(portal.MXID),
		}},
	})
	if err != nil {
		portal.log.Warnfln("Failed to disable notifications for %s while backfilling: %v", user.MXID, err)
	}
}

func (portal *Portal) enableNotifications(user *User) {
	if !portal.bridge.Config.Bridge.HistoryDisableNotifs {
		return
	}
	puppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
	if puppet == nil || puppet.customIntent == nil {
		return
	}
	portal.log.Debugfln("Re-enabling notifications for %s after backfilling", user.MXID)
	ruleID := fmt.Sprintf("dev.spherics.silence_while_backfilling.%s", portal.MXID)
	err := puppet.customIntent.DeletePushRule("global", pushrules.OverrideRule, ruleID)
	if err != nil {
		portal.log.Warnfln("Failed to re-enable notifications for %s after backfilling: %v", user.MXID, err)
	}
}

func (portal *Portal) FillInitialHistory(user *User) error {
	if portal.bridge.Config.Bridge.InitialHistoryFill == 0 {
		return nil
	}
	endBackfill := portal.beginBackfill()
	defer endBackfill()
	if portal.privateChatBackfillInvitePuppet != nil {
		portal.privateChatBackfillInvitePuppet()
	}

	// n := portal.bridge.Config.Bridge.InitialHistoryFill
	n := 70
	portal.log.Infoln("Filling initial history, maximum", n, "messages")
	// portal.log.Debugfln("Fetching chunk %d (%d messages / %d cap) before message %s", chunkNum, count, n, before)
	chat, ok := user.Pulse.Store.Chats[portal.Key.PID]
	if !ok {
		return fmt.Errorf("chat not found for pid: %s", portal.Key.PID)
	}

	convoID, err := strconv.Atoi(chat.ID)
	if err != nil {
		return fmt.Errorf("invalid chat id")
	}

	messages, err := user.Pulse.GetMessages(convoID, 0)
	if err != nil {
		return err
	}
	portal.log.Debugfln("Fetched chunk and received %d messages", len(messages))

	portal.disableNotifications(user)
	portal.handleHistory(user, messages)
	portal.enableNotifications(user)
	portal.log.Infoln("Initial history fill complete")
	return nil
}

func (portal *Portal) handleHistory(user *User, messages []pulsesms.Message) {
	portal.log.Infoln("Handling", len(messages), "messages of history")
	for _, message := range messages {

		if portal.privateChatBackfillInvitePuppet != nil && portal.IsPrivateChat() {
			portal.privateChatBackfillInvitePuppet()
		}

		portal.handleMessage(PortalMessage{portal.Key.PID, user, message, uint64(message.Timestamp)}, true)
	}
}

type BridgeInfoSection struct {
	ID          string              `json:"id"`
	DisplayName string              `json:"displayname,omitempty"`
	AvatarURL   id.ContentURIString `json:"avatar_url,omitempty"`
	ExternalURL string              `json:"external_url,omitempty"`
}

type BridgeInfoContent struct {
	BridgeBot id.UserID          `json:"bridgebot"`
	Creator   id.UserID          `json:"creator,omitempty"`
	Protocol  BridgeInfoSection  `json:"protocol"`
	Network   *BridgeInfoSection `json:"network,omitempty"`
	Channel   BridgeInfoSection  `json:"channel"`
}

var (
	StateBridgeInfo         = event.Type{Type: "m.bridge", Class: event.StateEventType}
	StateHalfShotBridgeInfo = event.Type{Type: "uk.half-shot.bridge", Class: event.StateEventType}
)

func (portal *Portal) getBridgeInfo() (string, BridgeInfoContent) {
	bridgeInfo := BridgeInfoContent{
		BridgeBot: portal.bridge.Bot.UserID,
		Creator:   portal.MainIntent().UserID,
		Protocol: BridgeInfoSection{
			ID:          "pulsesms",
			DisplayName: "PulseSMS",
			AvatarURL:   id.ContentURIString(portal.bridge.Config.AppService.Bot.Avatar),
			ExternalURL: "https://home.pulsesms.app/",
		},
		Channel: BridgeInfoSection{
			ID:          portal.Key.PID,
			DisplayName: portal.Name,
			AvatarURL:   portal.AvatarURL.CUString(),
		},
	}
	bridgeInfoStateKey := fmt.Sprintf("dev.spherics.pulsesms://pulsesms/%s", portal.Key.PID)
	return bridgeInfoStateKey, bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debugln("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, StateBridgeInfo, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update m.bridge:", err)
	}
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, StateHalfShotBridgeInfo, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update uk.half-shot.bridge:", err)
	}
}

func (portal *Portal) CreateMatrixRoom(user *User) error {
	portal.log.Infofln("Creating Matrix room. source: %s, portal pid: %s:", user.MXID, portal.Key.PID)
	portal.log.Infofln("Portal name: %s, portal key %s", portal.Name, portal.Key)

	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if len(portal.MXID) > 0 {
		return nil
	}

	intent := portal.MainIntent()
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}

	portal.log.Infoln("Creating Matrix room. Info source:", user.MXID)

	//TODO
	chat, ok := user.Pulse.GetChat(portal.Key.PID)
	if !ok {
		return fmt.Errorf("no chat for portal pid: %s", portal.Key.PID)
	}

	var broadcastMetadata []pulsesms.PhoneNumber

	//
	if portal.IsPrivateChat() {
		puppet := portal.bridge.GetPuppetByPID(portal.Key.PID)
		puppet.SyncContactIfNecessary(user)
		if portal.bridge.Config.Bridge.PrivateChatPortalMeta {
			portal.Name = puppet.Displayname
			portal.AvatarURL = puppet.AvatarURL
			portal.Avatar = puppet.Avatar
		} else {
			portal.Name = ""
		}
		portal.Topic = "PulseSMS private chat"
	} else {
		// var err error
		// metadata, ok := user.Pulse.Store.Chats[portal.Key.PID]
		// if ok && metadata.Status == 0 {
		// portal.Name = metadata.Name
		// portal.Topic = metadata.Topic
		// }
		portal.UpdateAvatar(user, nil, false)
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()

	initialState := []*event.Event{{
		Type: event.StatePowerLevels,
		Content: event.Content{
			Parsed: portal.GetBasePowerLevels(),
		},
	}, {
		Type:     StateBridgeInfo,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     StateHalfShotBridgeInfo,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}
	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{
				Parsed: event.RoomAvatarEventContent{URL: portal.AvatarURL},
			},
		})
	}

	invite := []id.UserID{user.MXID}
	if user.IsRelaybot {
		invite = portal.bridge.Config.Bridge.Relaybot.InviteUsers
	}

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1},
			},
		})
		portal.Encrypted = true
		if portal.IsPrivateChat() {
			invite = append(invite, portal.bridge.Bot.UserID)
		}
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility: "private",
		Name:       portal.Name,
		Topic:      portal.Topic,
		Invite:     invite,
		Preset:     "private_chat",
		// IsDirect:     portal.IsPrivateChat(),
		IsDirect:     len(chat.Members) == 1,
		InitialState: initialState,
	})
	if err != nil {
		return err
	}
	portal.MXID = resp.RoomID
	portal.Update()
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()

	// We set the memberships beforehand to make sure the encryption key exchange in initial backfill knows the users are here.
	for _, user := range invite {
		portal.bridge.StateStore.SetMembership(portal.MXID, user, event.MembershipInvite)
	}

	if chat.ID != "" {
		portal.SyncParticipants(user, chat.Members)
		// if metadata.Announce {
		// 	portal.RestrictMessageSending(metadata.Announce)
		// }
	} else if !user.IsRelaybot {
		customPuppet := portal.bridge.GetPuppetByCustomMXID(user.MXID)
		if customPuppet != nil && customPuppet.CustomIntent() != nil {
			_ = customPuppet.CustomIntent().EnsureJoined(portal.MXID)
		}
	}
	if broadcastMetadata != nil {
		portal.SyncBroadcastRecipients(user, broadcastMetadata)
	}
	inSpace := user.addPortalToSpace(portal)
	if portal.IsPrivateChat() && !user.IsRelaybot {
		puppet := user.bridge.GetPuppetByPID(portal.Key.PID)
		user.addPuppetToSpace(puppet)

		if portal.bridge.Config.Bridge.Encryption.Default {
			err = portal.bridge.Bot.EnsureJoined(portal.MXID)
			if err != nil {
				portal.log.Errorln("Failed to join created portal with bridge bot for e2be:", err)
			}
		}

		user.UpdateDirectChats(map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}})
	}

	user.CreateUserPortal(database.PortalKeyWithMeta{PortalKey: portal.Key, InCommunity: inSpace})

	err = portal.FillInitialHistory(user)
	if err != nil {
		portal.log.Errorln("Failed to fill history:", err)
	}
	return nil
}

func (portal *Portal) IsPrivateChat() bool {
	return false
	// return true
	// if portal.isPrivate == nil {
	// 	val := strings.HasSuffix(portal.Key.PID, whatsapp.NewUserSuffix)
	// 	portal.isPrivate = &val
	// }
	// return *portal.isPrivate
}

func (portal *Portal) HasRelaybot() bool {
	if portal.bridge.Relaybot == nil {
		return false
	} else if portal.hasRelaybot == nil {
		val := portal.bridge.Relaybot.IsInPortal(portal.Key)
		portal.hasRelaybot = &val
	}
	return *portal.hasRelaybot
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetByPID(portal.Key.PID).DefaultIntent()
	}
	return portal.bridge.Bot
}

func (portal *Portal) sendMainIntentMessage(content interface{}) (*mautrix.RespSendEvent, error) {
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, 0)
}

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	portal.log.Debugfln("sending message")
	wrappedContent := event.Content{Parsed: content}
	if timestamp != 0 && intent.IsCustomPuppet {
		wrappedContent.Raw = map[string]interface{}{
			"dev.spherics.pulsesms.puppet": intent.IsCustomPuppet,
		}
	}
	if portal.Encrypted && portal.bridge.Crypto != nil {
		// TODO maybe the locking should be inside mautrix-go?
		portal.encryptLock.Lock()
		encrypted, err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, wrappedContent)
		portal.encryptLock.Unlock()
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
		wrappedContent.Parsed = encrypted
	}
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) HandleTextMessage(source *User, message pulsesms.Message) {
	if message.ID == 0 {
		message.ID = message.DeviceID
	}

	intent := portal.startHandling(source, message)
	if intent == nil {
		return
	}

	content := &event.MessageEventContent{
		Body:    message.Data,
		MsgType: event.MsgText,
	}

	portal.bridge.Formatter.ParsePulse(content, []string{})
	// portal.SetReply(content, message.ContextInfo)

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	resp, err := portal.sendMessage(intent, event.EventMessage, content, int64(message.Timestamp))
	if err != nil {
		portal.log.Errorfln("Failed to handle message %s: %v", message.ID, err)
		return
	}
	portal.finishHandling(source, message, resp.EventID)
}

func (portal *Portal) sendMediaBridgeFailure(source *User, intent *appservice.IntentAPI, info pulsesms.Message, bridgeErr error) {
	portal.log.Errorfln("Failed to bridge media for %s: %v", info.ID, bridgeErr)
	resp, err := portal.sendMessage(intent, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    "Failed to bridge media",
	}, int64(info.Timestamp*1000))
	if err != nil {
		portal.log.Errorfln("Failed to send media download error message for %s: %v", info.ID, err)
	} else {
		portal.finishHandling(source, info, resp.EventID)
	}
}

func (portal *Portal) encryptFile(data []byte, mimeType string) ([]byte, string, *event.EncryptedFileInfo) {
	if !portal.Encrypted {
		return data, mimeType, nil
	}

	file := &event.EncryptedFileInfo{
		EncryptedFile: *attachment.NewEncryptedFile(),
		URL:           "",
	}
	return file.Encrypt(data), "application/octet-stream", file
}

func (portal *Portal) tryKickUser(userID id.UserID, intent *appservice.IntentAPI) error {
	_, err := intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: userID})
	if err != nil {
		httpErr, ok := err.(mautrix.HTTPError)
		if ok && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_FORBIDDEN" {
			_, err = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: userID})
		}
	}
	return err
}

func (portal *Portal) removeUser(isSameUser bool, kicker *appservice.IntentAPI, target id.UserID, targetIntent *appservice.IntentAPI) {
	if !isSameUser || targetIntent == nil {
		err := portal.tryKickUser(target, kicker)
		if err != nil {
			portal.log.Warnfln("Failed to kick %s from %s: %v", target, portal.MXID, err)
			if targetIntent != nil {
				_, _ = targetIntent.LeaveRoom(portal.MXID)
			}
		}
	} else {
		_, err := targetIntent.LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to leave portal as %s: %v", target, err)
			_, _ = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: target})
		}
	}
}

type base struct {
	download func() ([]byte, error)
	// info     whatsapp.MessageInfo
	// context  whatsapp.ContextInfo
	mimeType string
}

type mediaMessage struct {
	base

	thumbnail     []byte
	caption       string
	fileName      string
	length        uint32
	sendAsSticker bool
}

// TODO
func (portal *Portal) HandleMediaMessage(source *User, msg mediaMessage) {
	return
}

func makeMessageID() *string {
	b := make([]byte, 10)
	rand.Read(b)
	str := strings.ToUpper(hex.EncodeToString(b))
	return &str
}

func (portal *Portal) downloadThumbnail(content *event.MessageEventContent, id id.EventID) []byte {
	if len(content.GetInfo().ThumbnailURL) == 0 {
		return nil
	}
	mxc, err := content.GetInfo().ThumbnailURL.Parse()
	if err != nil {
		portal.log.Errorln("Malformed thumbnail URL in %s: %v", id, err)
	}
	thumbnail, err := portal.MainIntent().DownloadBytes(mxc)
	if err != nil {
		portal.log.Errorln("Failed to download thumbnail in %s: %v", id, err)
		return nil
	}
	thumbnailType := http.DetectContentType(thumbnail)
	var img image.Image
	switch thumbnailType {
	case "image/png":
		img, err = png.Decode(bytes.NewReader(thumbnail))
	case "image/gif":
		img, err = gif.Decode(bytes.NewReader(thumbnail))
	case "image/jpeg":
		return thumbnail
	default:
		return nil
	}
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, img, &jpeg.Options{
		Quality: jpeg.DefaultQuality,
	})
	if err != nil {
		portal.log.Errorln("Failed to re-encode thumbnail in %s: %v", id, err)
		return nil
	}
	return buf.Bytes()
}

func (portal *Portal) convertGifToVideo(gif []byte) ([]byte, error) {
	dir, err := ioutil.TempDir("", "gif-convert-*")
	if err != nil {
		return nil, fmt.Errorf("failed to make temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inputFile, err := os.OpenFile(filepath.Join(dir, "input.gif"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed open input file: %w", err)
	}
	_, err = inputFile.Write(gif)
	if err != nil {
		_ = inputFile.Close()
		return nil, fmt.Errorf("failed to write gif to input file: %w", err)
	}
	_ = inputFile.Close()

	outputFileName := filepath.Join(dir, "output.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "warning",
		"-f", "gif", "-i", inputFile.Name(),
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-movflags", "+faststart",
		"-filter:v", "crop='floor(in_w/2)*2:floor(in_h/2)*2'",
		outputFileName)
	vcLog := portal.log.Sub("VideoConverter").Writer(log.LevelWarn)
	cmd.Stdout = vcLog
	cmd.Stderr = vcLog

	err = cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to run ffmpeg: %w", err)
	}
	outputFile, err := os.OpenFile(filepath.Join(dir, "output.mp4"), os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open output file: %w", err)
	}
	defer func() {
		_ = outputFile.Close()
		_ = os.Remove(outputFile.Name())
	}()
	mp4, err := ioutil.ReadAll(outputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read mp4 from output file: %w", err)
	}
	return mp4, nil
}

// TODO use constants for pulse supported mimetypes
func (portal *Portal) preprocessMatrixMedia(sender *User, relaybotFormatted bool, content *event.MessageEventContent, eventID id.EventID, mediaType string) *MediaUpload {
	return &MediaUpload{}
}

type MediaUpload struct {
	Caption       string
	MentionedPIDs []pulsesms.PhoneNumber
	URL           string
	MediaKey      []byte
	FileEncSHA256 []byte
	FileSHA256    []byte
	FileLength    uint64
	Thumbnail     []byte
}

func (portal *Portal) sendMatrixConnectionError(sender *User, eventID id.EventID) bool {
	if !sender.HasSession() {
		portal.log.Debugln("Ignoring event", eventID, "from", sender.MXID, "as user has no session")
		return true
	} else if !sender.IsConnected() {
		inRoom := ""
		if portal.IsPrivateChat() {
			inRoom = " in your management room"
		}
		if sender.IsLoginInProgress() {
			// portal.log.Debugln("Waiting for connection before handling event", eventID, "from", sender.MXID)
			// sender.Conn.WaitForLogin()
			// if sender.IsConnected() {
			// 	return false
			// }
		}
		reconnect := fmt.Sprintf("Use `%s reconnect`%s to reconnect.", portal.bridge.Config.Bridge.CommandPrefix, inRoom)
		portal.log.Debugln("Ignoring event", eventID, "from", sender.MXID, "as user is not connected")
		msg := format.RenderMarkdown("\u26a0 You are not connected to PulseSMS, so your message was not bridged. "+reconnect, true, false)
		msg.MsgType = event.MsgNotice
		_, err := portal.sendMainIntentMessage(msg)
		if err != nil {
			portal.log.Errorln("Failed to send bridging failure message:", err)
		}
		return true
	}
	return false
}

func (portal *Portal) addRelaybotFormat(sender *User, content *event.MessageEventContent) bool {
	member := portal.MainIntent().Member(portal.MXID, sender.MXID)
	if len(member.Displayname) == 0 {
		member.Displayname = string(sender.MXID)
	}

	if content.Format != event.FormatHTML {
		content.FormattedBody = strings.Replace(html.EscapeString(content.Body), "\n", "<br/>", -1)
		content.Format = event.FormatHTML
	}
	data, err := portal.bridge.Config.Bridge.Relaybot.FormatMessage(content, sender.MXID, member)
	if err != nil {
		portal.log.Errorln("Failed to apply relaybot format:", err)
	}
	content.FormattedBody = data
	return true
}

func generatePulseMessageID() int {
	const min = 1
	const max = 922337203685477

	s := rand.Float64()
	x := s * (max - min + 1)

	return int(math.Floor(x) + min)
}

func (portal *Portal) convertMatrixMessage(sender *User, evt *event.Event) (*pulsesms.Message, *User) {
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		portal.log.Debugfln("Failed to handle event %s: unexpected parsed content type %T", evt.ID, evt.Content.Parsed)
		return nil, sender
	}

	convoID, err := strconv.Atoi(portal.Key.PID)
	if err != nil {
		portal.bridge.Log.Warnfln("portal ID is not valid convoID: %s", portal.Key.PID)
	}

    id := generatePulseMessageID()
	msg := &pulsesms.Message{
		ConversationID: convoID,
		From:           sender.PID,
		ID:             id,
	}
	replyToID := content.GetReplyTo()
	if len(replyToID) > 0 {
		content.RemoveReplyFallback()
	}
	relaybotFormatted := false
	if sender.NeedsRelaybot(portal) {
		if !portal.HasRelaybot() {
			if sender.HasSession() {
				portal.log.Debugln("Database says", sender.MXID, "not in chat and no relaybot, but trying to send anyway")
			} else {
				portal.log.Debugln("Ignoring message from", sender.MXID, "in chat with no relaybot")
				return nil, sender
			}
		} else {
			relaybotFormatted = portal.addRelaybotFormat(sender, content)
			sender = portal.bridge.Relaybot
		}
	}
	// if evt.Type == event.EventSticker {
	// 	content.MsgType = event.MsgImage
	// } else if content.MsgType == event.MsgImage && content.GetInfo().MimeType == "image/gif" {
	// 	content.MsgType = event.MsgVideo
	// }

	switch content.MsgType {
	case event.MsgText, event.MsgEmote, event.MsgNotice:
		text := content.Body
		if content.MsgType == event.MsgNotice && !portal.bridge.Config.Bridge.BridgeNotices {
			return nil, sender
		}
		// if content.Format == event.FormatHTML {
		// 	text, ctxInfo.Mentionedpid = portal.bridge.Formatter.ParseMatrix(content.FormattedBody)
		// }
		// if ctxInfo.StanzaId != nil || ctxInfo.Mentionedpid != nil {
		// 	info.Message.ExtendedTextMessage = &waProto.ExtendedTextMessage{
		// 		Text:        &text,
		// 		ContextInfo: ctxInfo,
		// 	}
		// } else {
		// 	info.Message.Conversation = &text
		// }
		msg.Data = text
	case event.MsgImage:
		media := portal.preprocessMatrixMedia(sender, relaybotFormatted, content, evt.ID, "image/png")
		if media == nil {
			return nil, sender
		}
		// TODO add image using mimetype
		// ctxInfo.Mentionedpid = media.MentionedPIDs
		// info.Message.ImageMessage = &waProto.ImageMessage{
		// 	ContextInfo:   ctxInfo,
		// 	Caption:       &media.Caption,
		// 	JpegThumbnail: media.Thumbnail,
		// 	Url:           &media.URL,
		// 	MediaKey:      media.MediaKey,
		// 	Mimetype:      &content.GetInfo().MimeType,
		// 	FileEncSha256: media.FileEncSHA256,
		// 	FileSha256:    media.FileSHA256,
		// 	FileLength:    &media.FileLength,
		// }
	case event.MsgVideo:
		// gifPlayback := content.GetInfo().MimeType == "image/gif"
		media := portal.preprocessMatrixMedia(sender, relaybotFormatted, content, evt.ID, "image/gif")
		if media == nil {
			return nil, sender
		}
		// duration := uint32(content.GetInfo().Duration)
		// ctxInfo.Mentionedpid = media.MentionedPIDs
		// info.Message.VideoMessage = &waProto.VideoMessage{
		// 	ContextInfo:   ctxInfo,
		// 	Caption:       &media.Caption,
		// 	JpegThumbnail: media.Thumbnail,
		// 	Url:           &media.URL,
		// 	MediaKey:      media.MediaKey,
		// 	Mimetype:      &content.GetInfo().MimeType,
		// 	GifPlayback:   &gifPlayback,
		// 	Seconds:       &duration,
		// 	FileEncSha256: media.FileEncSHA256,
		// 	FileSha256:    media.FileSHA256,
		// 	FileLength:    &media.FileLength,
		// }
	case event.MsgAudio:
		media := portal.preprocessMatrixMedia(sender, relaybotFormatted, content, evt.ID, "audio/mp3")
		if media == nil {
			return nil, sender
		}
		// duration := uint32(content.GetInfo().Duration)
		// info.Message.AudioMessage = &waProto.AudioMessage{
		// 	ContextInfo:   ctxInfo,
		// 	Url:           &media.URL,
		// 	MediaKey:      media.MediaKey,
		// 	Mimetype:      &content.GetInfo().MimeType,
		// 	Seconds:       &duration,
		// 	FileEncSha256: media.FileEncSHA256,
		// 	FileSha256:    media.FileSHA256,
		// 	FileLength:    &media.FileLength,
		// }
	// case event.MsgFile:
	// 	media := portal.preprocessMatrixMedia(sender, relaybotFormatted, content, evt.ID, whatsapp.MediaDocument)
	// 	if media == nil {
	// 		return nil, sender
	// 	}
	// 	info.Message.DocumentMessage = &waProto.DocumentMessage{
	// 		ContextInfo:   ctxInfo,
	// 		Url:           &media.URL,
	// 		Title:         &content.Body,
	// 		FileName:      &content.Body,
	// 		MediaKey:      media.MediaKey,
	// 		Mimetype:      &content.GetInfo().MimeType,
	// 		FileEncSha256: media.FileEncSHA256,
	// 		FileSha256:    media.FileSHA256,
	// 		FileLength:    &media.FileLength,
	// 	}
	default:
		portal.log.Debugfln("Unhandled Matrix event %s: unknown msgtype %s", evt.ID, content.MsgType)
		return nil, sender
	}
	return msg, sender
}

// TODO
func (portal *Portal) wasMessageSent(sender *User, id string) bool {
	// _, err := sender.Conn.LoadMessagesAfter(portal.Key.PID, id, true, 0)
	// if err != nil {
	// 	if err != whatsapp.ErrServerRespondedWith404 {
	// 		portal.log.Warnfln("Failed to check if message was bridged without response: %v", err)
	// 	}
	// 	return false
	// }
	return true
}

func (portal *Portal) sendErrorMessage(message string) id.EventID {
	resp, err := portal.sendMainIntentMessage(event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    fmt.Sprintf("\u26a0 Your message may not have been bridged: %v", message),
	})
	if err != nil {
		portal.log.Warnfln("Failed to send bridging error message:", err)
		return ""
	}
	return resp.EventID
}

func (portal *Portal) sendDeliveryReceipt(eventID id.EventID) {
	if portal.bridge.Config.Bridge.DeliveryReceipts {
		err := portal.bridge.Bot.MarkRead(portal.MXID, eventID)
		if err != nil {
			portal.log.Debugfln("Failed to send delivery receipt for %s: %v", eventID, err)
		}
	}
}

func (portal *Portal) HandleMatrixMessage(sender *User, evt *event.Event) {
	if !portal.HasRelaybot() && ((portal.IsPrivateChat() && sender.PID != portal.Key.Receiver) ||
		portal.sendMatrixConnectionError(sender, evt.ID)) {
		return
	}
	portal.log.Debugfln("Received event %s", evt.ID)
	msg, sender := portal.convertMatrixMessage(sender, evt)
	if msg == nil {
		return
	}
	dbMsg := portal.markHandled(sender, msg, evt.ID, false)
	portal.log.Debugln("Sending event", evt.ID, "to PulseSMS", msg.ID)
	portal.sendRaw(sender, evt, msg, dbMsg)
}

func (portal *Portal) sendRaw(sender *User, evt *event.Event, info *pulsesms.Message, dbMsg *database.Message) {
	errChan := make(chan error, 1)
	var err error

	// TODO refactor out to func
	go func() {
		err = sender.Pulse.SendMessage(*info, info.ChatID())
		if err != nil {
			portal.log.Errorfln("failed to send pulse msg: %v", err)
			errChan <- err
			return
		}
		errChan <- nil

	}()

	var errorEventID id.EventID
	select {
	case err = <-errChan:
	case <-time.After(time.Duration(portal.bridge.Config.Bridge.ConnectionTimeout) * time.Second):
		if portal.bridge.Config.Bridge.FetchMessageOnTimeout && portal.wasMessageSent(sender, fmt.Sprint(info.ID)) {
			portal.log.Debugln("Matrix event %s was bridged, but response didn't arrive within timeout")
			portal.sendDeliveryReceipt(evt.ID)
		} else {
			portal.log.Warnfln("Response when bridging Matrix event %s is taking long to arrive", evt.ID)
			errorEventID = portal.sendErrorMessage("message sending timed out")
		}
		err = <-errChan
	}
	if err != nil {
		portal.log.Errorfln("Error handling Matrix event %s: %v", evt.ID, err)
		portal.sendErrorMessage(err.Error())
	} else {
		portal.log.Debugfln("Handled Matrix event %s", evt.ID)
		portal.sendDeliveryReceipt(evt.ID)
		dbMsg.MarkSent()
	}
	if errorEventID != "" {
		_, err = portal.MainIntent().RedactEvent(portal.MXID, errorEventID)
		if err != nil {
			portal.log.Warnfln("Failed to redact timeout warning message %s: %v", errorEventID, err)
		}
	}
}

func (portal *Portal) HandleMatrixRedaction(sender *User, evt *event.Event) {
	return
}

func (portal *Portal) Delete() {
	portal.Portal.Delete()
	portal.bridge.portalsLock.Lock()
	delete(portal.bridge.portalsByPID, portal.Key)
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) GetMatrixUsers() ([]id.UserID, error) {
	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member list: %w", err)
	}
	var users []id.UserID
	for userID := range members.Joined {
		_, isPuppet := portal.bridge.ParsePuppetMXID(userID)
		if !isPuppet && userID != portal.bridge.Bot.UserID {
			users = append(users, userID)
		}
	}
	return users, nil
}

func (portal *Portal) CleanupIfEmpty() {
	users, err := portal.GetMatrixUsers()
	if err != nil {
		portal.log.Errorfln("Failed to get Matrix user list to determine if portal needs to be cleaned up: %v", err)
		return
	}

	if len(users) == 0 {
		portal.log.Infoln("Room seems to be empty, cleaning up...")
		portal.Delete()
		portal.Cleanup(false)
	}
}

func (portal *Portal) Cleanup(puppetsOnly bool) {
	if len(portal.MXID) == 0 {
		return
	}
	if portal.IsPrivateChat() {
		_, err := portal.MainIntent().LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}
	intent := portal.MainIntent()
	members, err := intent.JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorln("Failed to get portal members for cleanup:", err)
		return
	}
	for member := range members.Joined {
		if member == intent.UserID {
			continue
		}
		puppet := portal.bridge.GetPuppetByMXID(member)
		if puppet != nil {
			_, err = puppet.DefaultIntent().LeaveRoom(portal.MXID)
			if err != nil {
				portal.log.Errorln("Error leaving as puppet while cleaning up portal:", err)
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				portal.log.Errorln("Error kicking user while cleaning up portal:", err)
			}
		}
	}
	_, err = intent.LeaveRoom(portal.MXID)
	if err != nil {
		portal.log.Errorln("Error leaving with main intent while cleaning up portal:", err)
	}
}

func (portal *Portal) HandleMatrixLeave(sender *User) {
	if portal.IsPrivateChat() {
		portal.log.Debugln("User left private chat portal, cleaning up and deleting...")
		portal.Delete()
		portal.Cleanup(false)
		return
	}
	portal.CleanupIfEmpty()
}

func (portal *Portal) HandleMatrixKick(sender *User, evt *event.Event) {
	puppet := portal.bridge.GetPuppetByMXID(id.UserID(evt.GetStateKey()))
	if puppet != nil {
		// TODO?
		return
	}
}

func (portal *Portal) HandleMatrixInvite(sender *User, evt *event.Event) {
	puppet := portal.bridge.GetPuppetByMXID(id.UserID(evt.GetStateKey()))
	if puppet != nil {
		// TODO?
		// resp, err := sender.Conn.AddMember(portal.Key.PID, []string{puppet.PID})
		// if err != nil {
		// 	portal.log.Errorfln("Failed to add %s to group as %s: %v", puppet.PID, sender.MXID, err)
		// 	return
		// }
		// portal.log.Infofln("Add %s response: %s", puppet.PID, <-resp)
	}
}

func (portal *Portal) HandleMatrixMeta(sender *User, evt *event.Event) {
	var resp <-chan string
	var err error
	switch content := evt.Content.Parsed.(type) {
	// TODO update in pulse
	case *event.RoomNameEventContent:
		if content.Name == portal.Name {
			return
		}
		portal.Name = content.Name
	case *event.TopicEventContent:
		if content.Topic == portal.Topic {
			return
		}
		portal.Topic = content.Topic
	case *event.RoomAvatarEventContent:
		return
	}
	if err != nil {
		portal.log.Errorln("Failed to update metadata:", err)
	} else {
		out := <-resp
		portal.log.Debugln("Successfully updated metadata:", out)
	}
}
