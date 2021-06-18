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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skip2/go-qrcode"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/pushrules"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/treethought/matrix-pulsesms/database"
	"github.com/treethought/pulsesms"
)

type User struct {
	*database.User
	Pulse *pulsesms.Client

	bridge *Bridge
	log    log.Logger

	Admin               bool
	Whitelisted         bool
	RelaybotWhitelisted bool

	IsRelaybot bool

	ConnectionErrors int
	CommunityID      string
	SpaceID          id.RoomID

	cleanDisconnection  bool
	batteryWarningsSent int
	lastReconnection    int64
	pushName            string

	chatListReceived chan struct{}
	syncPortalsDone  chan struct{}

	messageInput  chan PortalMessage
	messageOutput chan PortalMessage

	syncStart chan struct{}
	syncWait  sync.WaitGroup
	syncing   int32

	mgmtCreateLock  sync.Mutex
	connLock        sync.Mutex
	cancelReconnect func()

	prevBridgeStatus *AsmuxPong
}

func (bridge *Bridge) GetUserByMXID(userID id.UserID) *User {
	_, isPuppet := bridge.ParsePuppetMXID(userID)
	if isPuppet || userID == bridge.Bot.UserID {
		return nil
	}
	bridge.usersLock.Lock()
	defer bridge.usersLock.Unlock()
	user, ok := bridge.usersByMXID[userID]
	if !ok {
		return bridge.loadDBUser(bridge.DB.User.GetByMXID(userID), &userID)
	}
	return user
}

func (bridge *Bridge) GetUserByPID(userID pulsesms.PhoneNumber) *User {
	bridge.usersLock.Lock()
	defer bridge.usersLock.Unlock()
	user, ok := bridge.usersByPID[userID]
	if !ok {
		return bridge.loadDBUser(bridge.DB.User.GetByPID(userID), nil)
	}
	return user
}

func (user *User) addToPIDMap() {
	user.bridge.usersLock.Lock()
	user.bridge.usersByPID[user.PID] = user
	user.bridge.usersLock.Unlock()
}

func (user *User) removeFromPIDMap() {
	user.bridge.usersLock.Lock()
	pidUser, ok := user.bridge.usersByPID[user.PID]
	if ok && user == pidUser {
		delete(user.bridge.usersByPID, user.PID)
	}
	user.bridge.usersLock.Unlock()
	user.bridge.Metrics.TrackLoginState(user.PID, false)
	user.sendBridgeStatus(AsmuxPong{Error: AsmuxWANotLoggedIn})
}

func (bridge *Bridge) GetAllUsers() []*User {
	bridge.usersLock.Lock()
	defer bridge.usersLock.Unlock()
	dbUsers := bridge.DB.User.GetAll()
	output := make([]*User, len(dbUsers))
	for index, dbUser := range dbUsers {
		user, ok := bridge.usersByMXID[dbUser.MXID]
		if !ok {
			user = bridge.loadDBUser(dbUser, nil)
		}
		output[index] = user
	}
	return output
}

func (bridge *Bridge) loadDBUser(dbUser *database.User, mxid *id.UserID) *User {
	if dbUser == nil {
		if mxid == nil {
			return nil
		}
		dbUser = bridge.DB.User.New()
		dbUser.MXID = *mxid
		dbUser.Insert()
	}
	user := bridge.NewUser(dbUser)
	bridge.usersByMXID[user.MXID] = user
	if len(user.PID) > 0 {
		bridge.usersByPID[user.PID] = user
	}
	if len(user.ManagementRoom) > 0 {
		bridge.managementRooms[user.ManagementRoom] = user
	}
	return user
}

func (user *User) GetPortals() []*Portal {
	keys := user.User.GetPortalKeys()
	portals := make([]*Portal, len(keys))

	user.bridge.portalsLock.Lock()
	for i, key := range keys {
		portal, ok := user.bridge.portalsByPID[key]
		if !ok {
			portal = user.bridge.loadDBPortal(user.bridge.DB.Portal.GetByPID(key), &key)
		}
		portals[i] = portal
	}
	user.bridge.portalsLock.Unlock()
	return portals
}

func (bridge *Bridge) NewUser(dbUser *database.User) *User {
	user := &User{
		User:   dbUser,
		bridge: bridge,
		log:    bridge.Log.Sub("User").Sub(string(dbUser.MXID)),

		IsRelaybot: false,

		chatListReceived: make(chan struct{}, 1),
		syncPortalsDone:  make(chan struct{}, 1),
		syncStart:        make(chan struct{}, 1),
		messageInput:     make(chan PortalMessage),
		messageOutput:    make(chan PortalMessage, bridge.Config.Bridge.UserMessageBuffer),
	}
	user.RelaybotWhitelisted = user.bridge.Config.Bridge.Permissions.IsRelaybotWhitelisted(user.MXID)
	user.Whitelisted = user.bridge.Config.Bridge.Permissions.IsWhitelisted(user.MXID)
	user.Admin = user.bridge.Config.Bridge.Permissions.IsAdmin(user.MXID)
	go user.handleMessageLoop()
	go user.runMessageRingBuffer()
	return user
}

func (user *User) GetManagementRoom() id.RoomID {
	if len(user.ManagementRoom) == 0 {
		user.mgmtCreateLock.Lock()
		defer user.mgmtCreateLock.Unlock()
		if len(user.ManagementRoom) > 0 {
			return user.ManagementRoom
		}
		resp, err := user.bridge.Bot.CreateRoom(&mautrix.ReqCreateRoom{
			Topic:    "PulseSMS bridge notices",
			IsDirect: true,
		})
		if err != nil {
			user.log.Errorln("Failed to auto-create management room:", err)
		} else {
			user.SetManagementRoom(resp.RoomID)
		}
	}
	return user.ManagementRoom
}

func (user *User) SetManagementRoom(roomID id.RoomID) {
	existingUser, ok := user.bridge.managementRooms[roomID]
	if ok {
		existingUser.ManagementRoom = ""
		existingUser.Update()
	}

	user.ManagementRoom = roomID
	user.bridge.managementRooms[user.ManagementRoom] = user
	user.Update()
}

func (user *User) SetSession(session *pulsesms.KeyCredentials) {
	user.log.Debugfln("setting session for pulse account %s", session.AccountID)
	if session == nil {
		user.Session = nil
		user.LastConnection = 0
	} else if len(session.AccountID) > 0 {
		user.Session = session
	} else {
		return
	}
	user.Update()
}

func (user *User) Connect(evenIfNoSession bool) bool {
	user.log.Info("connecting user")

	// TODO
	// user.connLock.Lock()
	// if user.Pulse != nil {
	// 	user.connLock.Unlock()
	// 	if user.Pulse.IsConnected() {
	// 		return true
	// 	} else {
	// 		return user.RestoreSession()
	// 	}
	// } else if !evenIfNoSession && user.Session == nil {
	// 	user.connLock.Unlock()
	// 	return false
	// }
	// user.log.Debugln("Connecting to Pulse")
	// if user.Session != nil {
	// 	user.sendBridgeStatus(AsmuxPong{Error: AsmuxWAConnecting})
	// }
	// timeout := time.Duration(user.bridge.Config.Bridge.ConnectionTimeout)
	// if timeout == 0 {
	// 	timeout = 20
	// }

	// TODO: maybe stream and check connection here

	user.connLock.Lock()
	if user.Pulse == nil {
		user.Pulse = pulsesms.New()
	}

	if user.Session == nil {
		user.bridge.Bot.SendText(user.ManagementRoom, "credentials are not set, please login")
		return false
	}

	creds := pulsesms.KeyCredentials{
		AccountID:    user.Session.AccountID,
		PasswordHash: user.Session.PasswordHash,
		Salt:         user.Session.Salt,
	}
	err := user.Pulse.GenerateKey(creds)
	if err != nil {
		user.bridge.Bot.SendText(user.ManagementRoom, "failed to connnect with pulse, please login again")
		return false
	}

	user.Pulse.SetMessageHandler(func(m pulsesms.Message) {
		user.HandleEvent(m)
	})

	user.log.Infofln("starting pulse stream")
	go func() {
		for {
			if !user.Pulse.IsConnected() {
				user.sendBridgeNotice("pulse connection has been lost, reconnecting...")
				go func() {
					for {
						if user.Pulse.IsConnected() {
							user.sendBridgeNotice("reconnected successfully")
							return
						}
					}
				}()
				user.Pulse.Stream()
			}
		}
	}()
	user.setupAdminTestHooks()
	user.connLock.Unlock()
	return true
	// return user.RestoreSession()
}

func (user *User) DeleteConnection() {
	user.connLock.Lock()
	if user.Pulse == nil {
		user.connLock.Unlock()
		return
	}
	user.Pulse.Disconnect()
	user.bridge.Metrics.TrackConnectionState(user.PID, false)
	user.sendBridgeStatus(AsmuxPong{Error: AsmuxWANotConnected})
	user.connLock.Unlock()
}

func (user *User) RestoreSession() bool {
	//TODO
	if user.Session == nil {
		return false
	}

	return false

	//if user.Session != nil {
	//	user.Conn.SetSession(*user.Session)
	//	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	//	defer cancel()
	//	err := user.Conn.Restore(true, ctx)
	//	if err == whatsapp.ErrAlreadyLoggedIn {
	//		return true
	//	} else if err != nil {
	//		user.log.Errorln("Failed to restore session:", err)
	//		if errors.Is(err, whatsapp.ErrUnpaired) {
	//			user.sendMarkdownBridgeAlert("\u26a0 Failed to connect to WhatsApp: unpaired from phone. " +
	//				"To re-pair your phone, log in again.")
	//			user.removeFromPIDMap()
	//			//user.PID = ""
	//			user.SetSession(nil)
	//			user.DeleteConnection()
	//			return false
	//		} else {
	//			user.sendBridgeStatus(AsmuxPong{Error: AsmuxWANotConnected})
	//			user.sendMarkdownBridgeAlert("\u26a0 Failed to connect to WhatsApp. Make sure WhatsApp " +
	//				"on your phone is reachable and use `reconnect` to try connecting again.")
	//		}
	//		user.log.Debugln("Disconnecting due to failed session restore...")
	//		err = user.Conn.Disconnect()
	//		if err != nil {
	//			user.log.Errorln("Failed to disconnect after failed session restore:", err)
	//		}
	//		return false
	//	}
	//	user.ConnectionErrors = 0
	//	user.log.Debugln("Session restored successfully")
	//	user.PostLogin()
	//}
	//return true
}

func (user *User) HasSession() bool {
	return user.Session != nil
}

func (user *User) IsConnected() bool {
	return user.Pulse != nil && user.Pulse.IsConnected() && user.Session != nil
}

func (user *User) IsLoginInProgress() bool {
	return false
	// return user.Conn != nil && user.Conn.IsLoginInProgress()
}

func (user *User) loginQrChannel(ce *CommandEvent, qrChan <-chan string, eventIDChan chan<- id.EventID) {
	var qrEventID id.EventID
	for code := range qrChan {
		if code == "stop" {
			return
		}
		qrCode, err := qrcode.Encode(code, qrcode.Low, 256)
		if err != nil {
			user.log.Errorln("Failed to encode QR code:", err)
			ce.Reply("Failed to encode QR code: %v", err)
			return
		}

		bot := user.bridge.AS.BotClient()

		resp, err := bot.UploadBytes(qrCode, "image/png")
		if err != nil {
			user.log.Errorln("Failed to upload QR code:", err)
			ce.Reply("Failed to upload QR code: %v", err)
			return
		}

		if qrEventID == "" {
			sendResp, err := bot.SendImage(ce.RoomID, code, resp.ContentURI)
			if err != nil {
				user.log.Errorln("Failed to send QR code to user:", err)
				return
			}
			qrEventID = sendResp.EventID
			eventIDChan <- qrEventID
		} else {
			_, err = bot.SendMessageEvent(ce.RoomID, event.EventMessage, &event.MessageEventContent{
				MsgType: event.MsgImage,
				Body:    code,
				URL:     resp.ContentURI.CUString(),
				NewContent: &event.MessageEventContent{
					MsgType: event.MsgImage,
					Body:    code,
					URL:     resp.ContentURI.CUString(),
				},
				RelatesTo: &event.RelatesTo{
					Type:    event.RelReplace,
					EventID: qrEventID,
				},
			})
			if err != nil {
				user.log.Errorln("Failed to send edited QR code to user:", err)
			}
		}
	}
}

func (user *User) Login(ce *CommandEvent) {
	if len(ce.Args) < 3 {
		ce.Reply("invalid usage:\vuse login username password phonenumber")
	}

	username, pw, number := ce.Args[0], ce.Args[1], ce.Args[2]
	user.bridge.Log.Debugf("Logging into pulse as %s", username)

	// TODO maybe do this with custom puppet
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		ce.Reply("invalid phone number")
		return
	}
	cleanID := reg.ReplaceAllString(number, "")
	user.log.Infofln("setting user PID to %s, was %s", cleanID, user.PID)
	user.PID = cleanID

	if user.Pulse == nil {
		user.Pulse = pulsesms.New()
	}

	creds := pulsesms.BasicCredentials{Username: username, Password: pw}
	err = user.Pulse.Login(creds)
	if err != nil {
		ce.Reply("failed to login")
		return
	}

	user.log.Debugln("Successful login as", username, "via command")
	user.ConnectionErrors = 0
	session := user.Pulse.GetKeyCredentials()
	// TODO there's a bit of duplication between this and the provisioning API login method
	//      Also between the two logout methods (commands.go and provisioning.go)
	// user.log.Debugln("Successful login as", pid, "via command")
	user.ConnectionErrors = 0
	// user.PID = strings.Replace(pid, whatsapp.OldUserSuffix, whatsapp.NewUserSuffix, 1)
	// user.addToPIDMap()

	user.SetSession(&session)
	ce.Reply("Successfully logged in, synchronizing chats...")
	user.PostLogin()
}

type Chat struct {
	pulsesms.Chat
	Portal  *Portal
	Contact pulsesms.Contact
}

type ChatList []Chat

func (cl ChatList) Len() int {
	return len(cl)
}

func (cl ChatList) Less(i, j int) bool {
	return cl[i].LastMessageTime > cl[j].LastMessageTime
}

func (cl ChatList) Swap(i, j int) {
	cl[i], cl[j] = cl[j], cl[i]
}

func (user *User) PostLogin() {
	user.sendBridgeStatus(AsmuxPong{OK: true})
	user.bridge.Metrics.TrackConnectionState(user.PID, true)
	user.bridge.Metrics.TrackLoginState(user.PID, true)
	user.bridge.Metrics.TrackBufferLength(user.MXID, len(user.messageOutput))
	if !atomic.CompareAndSwapInt32(&user.syncing, 0, 1) {
		// TODO we should cleanly stop the old sync and start a new one instead of not starting a new one
		user.log.Warnln("There seems to be a post-sync already in progress, not starting a new one")
		return
	}
	user.log.Debugln("Locking processing of incoming messages and starting post-login sync")
	user.chatListReceived = make(chan struct{}, 1)
	user.syncPortalsDone = make(chan struct{}, 1)
	user.syncWait.Add(1)
	user.syncStart <- struct{}{}
	go user.intPostLogin()
}

func (user *User) tryAutomaticDoublePuppeting() {
	if !user.bridge.Config.CanDoublePuppet(user.MXID) {
		return
	}
	user.log.Debugln("Checking if double puppeting needs to be enabled")
	puppet := user.bridge.GetPuppetByPID(user.PID)
	if len(puppet.CustomMXID) > 0 {
		user.log.Debugln("User already has double-puppeting enabled")
		// Custom puppet already enabled
		return
	}
	accessToken, err := puppet.loginWithSharedSecret(user.MXID)
	if err != nil {
		user.log.Warnln("Failed to login with shared secret:", err)
		return
	}
	err = puppet.SwitchCustomMXID(accessToken, user.MXID)
	if err != nil {
		puppet.log.Warnln("Failed to switch to auto-logined custom puppet:", err)
		return
	}
	user.log.Infoln("Successfully automatically enabled custom puppet")
}

func (user *User) sendBridgeNotice(formatString string, args ...interface{}) {
	notice := fmt.Sprintf(formatString, args...)
	_, err := user.bridge.Bot.SendNotice(user.GetManagementRoom(), notice)
	if err != nil {
		user.log.Warnf("Failed to send bridge notice \"%s\": %v", notice, err)
	}
}

func (user *User) sendMarkdownBridgeAlert(formatString string, args ...interface{}) {
	notice := fmt.Sprintf(formatString, args...)
	content := format.RenderMarkdown(notice, true, false)
	_, err := user.bridge.Bot.SendMessageEvent(user.GetManagementRoom(), event.EventMessage, content)
	if err != nil {
		user.log.Warnf("Failed to send bridge alert \"%s\": %v", notice, err)
	}
}

func (user *User) postConnPing() bool {
	user.log.Debugln("Making post-connection ping")
	// user.log.Debugln("NOT IMPLEMENTED")
	// var err error
	// for i := 0; ; i++ {
	// 	err = user.Conn.AdminTest()
	// 	if err == nil {
	// 		user.log.Debugln("Post-connection ping OK")
	// 		return true
	// 	} else if errors.Is(err, whatsapp.ErrConnectionTimeout) && i < 5 {
	// 		user.log.Warnfln("Post-connection ping timed out, sending new one")
	// 	} else {
	// 		break
	// 	}
	// }
	// user.log.Errorfln("Post-connection ping failed: %v. Disconnecting and then reconnecting after a second", err)
	// disconnectErr := user.Conn.Disconnect()
	// if disconnectErr != nil {
	// 	user.log.Warnln("Error while disconnecting after failed post-connection ping:", disconnectErr)
	// }
	// user.sendBridgeStatus(AsmuxPong{Error: AsmuxWANotConnected})
	// user.bridge.Metrics.TrackDisconnection(user.MXID)
	// go func() {
	// 	time.Sleep(1 * time.Second)
	// 	user.tryReconnect(fmt.Sprintf("Post-connection ping failed: %v", err))
	// }()
	return false
}

func (user *User) intPostLogin() {

	go func() {
		user.log.Debugfln("intPostLogin syncing...")
		chatList := []pulsesms.Chat{}
		err := user.Pulse.Sync()
		if err != nil {
			return
		}
		user.log.Infofln("finished syncing pulse")

		for _, chat := range user.Pulse.Store.Chats {
			// user.log.Debugfln("init post: %v", chat.ID)
			chatList = append(chatList, chat)
		}
		user.HandleChatList(chatList)
	}()

	defer atomic.StoreInt32(&user.syncing, 0)
	defer user.syncWait.Done()
	user.lastReconnection = time.Now().Unix()
	// user.createCommunity()
	user.createSpace()
	user.tryAutomaticDoublePuppeting()

	user.log.Debugln("Waiting for chat list receive confirmation")
	select {
	case <-user.chatListReceived:
		user.log.Debugln("Chat list receive confirmation received in PostLogin")
	case <-time.After(time.Duration(user.bridge.Config.Bridge.ChatListWait) * time.Second):
		user.log.Warnln("Timed out waiting for chat list to arrive!")
		user.postConnPing()
		return
	}

	if !user.postConnPing() {
		user.log.Debugln("Post-connection ping failed, unlocking processing of incoming messages.")
		return
	}

	user.log.Debugln("Waiting for portal sync complete confirmation")
	select {
	case <-user.syncPortalsDone:
		user.log.Debugln("Post-connection portal sync complete, unlocking processing of incoming messages.")
	// TODO this is too short, maybe a per-portal duration?
	case <-time.After(time.Duration(user.bridge.Config.Bridge.PortalSyncWait) * time.Second):
		user.log.Warnln("Timed out waiting for portal sync to complete! Unlocking processing of incoming messages.")
	}
}

type NormalMessage interface {
	// GetInfo() whatsapp.MessageInfo
	GetInfo() pulsesms.Message
}

func (user *User) HandleEvent(event interface{}) {
	switch v := event.(type) {
	// TODO prbly need to configure message types
	case pulsesms.Message:
		user.log.Infofln("handling pulse message")
		chat, ok := user.Pulse.GetChat(v.ChatID())
		if !ok {
			user.log.Warnfln("could not retrieve chat for received message, chatID: %s", v.ChatID())
			return
		}
		user.messageInput <- PortalMessage{chat.ID, user, v, uint64(v.Timestamp)}
	default:
		user.log.Debugfln("Unknown type of event in HandleEvent: %T", v)
	}
}

func (user *User) HandleStreamEvent(evt pulsesms.Message) {
	user.log.Infofln("Stream event: %+v", evt)
}

func (user *User) HandleChatList(chats []pulsesms.Chat) {
	user.log.Infofln("handling chat list with length: %d", len(chats))

	select {
	case user.chatListReceived <- struct{}{}:
	default:
	}
	go user.syncPortals(user.Pulse.Store.Chats, false)
}

func (user *User) updateChatMute(intent *appservice.IntentAPI, portal *Portal, mutedUntil int64) {
	if len(portal.MXID) == 0 || !user.bridge.Config.Bridge.MuteBridging {
		return
	} else if intent == nil {
		doublePuppet := user.bridge.GetPuppetByCustomMXID(user.MXID)
		if doublePuppet == nil || doublePuppet.CustomIntent() == nil {
			return
		}
		intent = doublePuppet.CustomIntent()
	}
	var err error
	if mutedUntil != -1 && mutedUntil < time.Now().Unix() {
		user.log.Debugfln("Portal %s is muted until %d, unmuting...", portal.MXID, mutedUntil)
		err = intent.DeletePushRule("global", pushrules.RoomRule, string(portal.MXID))
	} else {
		user.log.Debugfln("Portal %s is muted until %d, muting...", portal.MXID, mutedUntil)
		err = intent.PutPushRule("global", pushrules.RoomRule, string(portal.MXID), &mautrix.ReqPutPushRule{
			Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		})
	}
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		user.log.Warnfln("Failed to update push rule for %s through double puppet: %v", portal.MXID, err)
	}
}

type CustomTagData struct {
	Order        json.Number `json:"order"`
	DoublePuppet bool        `json:"dev.spherics.pulsesms.puppet"`
}

type CustomTagEventContent struct {
	Tags map[string]CustomTagData `json:"tags"`
}

func (user *User) updateChatTag(intent *appservice.IntentAPI, portal *Portal, tag string, active bool) {
	if len(portal.MXID) == 0 || len(tag) == 0 {
		return
	} else if intent == nil {
		doublePuppet := user.bridge.GetPuppetByCustomMXID(user.MXID)
		if doublePuppet == nil || doublePuppet.CustomIntent() == nil {
			return
		}
		intent = doublePuppet.CustomIntent()
	}
	var existingTags CustomTagEventContent
	err := intent.GetTagsWithCustomData(portal.MXID, &existingTags)
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		user.log.Warnfln("Failed to get tags of %s: %v", portal.MXID, err)
	}
	currentTag, ok := existingTags.Tags[tag]
	if active && !ok {
		user.log.Debugln("Adding tag", tag, "to", portal.MXID)
		data := CustomTagData{"0.5", true}
		err = intent.AddTagWithCustomData(portal.MXID, tag, &data)
	} else if !active && ok && currentTag.DoublePuppet {
		user.log.Debugln("Removing tag", tag, "from", portal.MXID)
		err = intent.RemoveTag(portal.MXID, tag)
	} else {
		err = nil
	}
	if err != nil {
		user.log.Warnfln("Failed to update tag %s for %s through double puppet: %v", tag, portal.MXID, err)
	}
}

type CustomReadReceipt struct {
	Timestamp    int64 `json:"ts,omitempty"`
	DoublePuppet bool  `json:"dev.spherics.pulsesms.puppet,omitempty"`
}

func (user *User) syncChatDoublePuppetDetails(doublePuppet *Puppet, chat Chat, justCreated bool) {
	if doublePuppet == nil || doublePuppet.CustomIntent() == nil {
		return
	}
	// intent := doublePuppet.CustomIntent()
	// if chat.UnreadCount == 0 && (justCreated || !user.bridge.Config.Bridge.MarkReadOnlyOnCreate) {
	// 	lastMessage := user.bridge.DB.Message.GetLastInChatBefore(chat.Portal.Key, chat.ReceivedAt.Unix())
	// 	if lastMessage != nil {
	// 		err := intent.MarkReadWithContent(chat.Portal.MXID, lastMessage.MXID, &CustomReadReceipt{DoublePuppet: true})
	// 		if err != nil {
	// 			user.log.Warnln("Failed to mark %s in %s as read after backfill: %v", lastMessage.MXID, chat.Portal.MXID, err)
	// 		}
	// 	}
	// } else if chat.UnreadCount == -1 {
	// 	user.log.Debugfln("Invalid unread count (missing field?) in chat info %+v", chat.Source)
	// }

	// if justCreated || !user.bridge.Config.Bridge.TagOnlyOnCreate {
	// user.updateChatMute(intent, chat.Portal, chat.MutedUntil)
	// user.updateChatTag(intent, chat.Portal, user.bridge.Config.Bridge.ArchiveTag, chat.IsArchived)
	// user.updateChatTag(intent, chat.Portal, user.bridge.Config.Bridge.PinnedTag, chat.IsPinned)
	// }
}

func (user *User) syncPortal(chat Chat) {
	user.log.Infofln("syncing portal for chat %s", chat.Name)
	// Don't sync unless chat meta sync is enabled or portal doesn't exist
	if user.bridge.Config.Bridge.ChatMetaSync || len(chat.Portal.MXID) == 0 {
		chat.Portal.Sync(user, chat.Chat)
	}
	// TODO: get last message time insntead of now
	err := chat.Portal.BackfillHistory(user, time.Now().Unix()-86400)
	if err != nil {
		chat.Portal.log.Errorln("Error backfilling history:", err)
	}
}

func (user *User) collectChatList(chatMap map[pulsesms.ChatID]pulsesms.Chat) ChatList {
	if chatMap == nil {
		chatMap = user.Pulse.Store.Chats
	}
	user.log.Infoln("collecting chat list")
	chats := make(ChatList, 0, len(chatMap))
	existingKeys := user.GetInCommunityMap()
	portalKeys := make([]database.PortalKeyWithMeta, 0, len(chatMap))
	for _, chat := range chatMap {
		user.log.Debugfln("handling chat %s, pid: %s", chat.Name, chat.ID)
		user.log.Debugfln("members: %v", chat.Members)

		portal := user.GetPortalByPID(chat.ID)

		// TODO group chats instead of first member?
		// or can prbly read from Store contacts directly
		firstMember := chat.Members[0]
		// portal := user.GetPortalByPID(firstMember)

		chats = append(chats, Chat{
			Chat:    chat,
			Portal:  portal,
			Contact: user.Pulse.Store.Contacts[firstMember],
		})

		user.log.Debugfln("adding chat for contact %s", user.Pulse.Store.Contacts[firstMember].Name)
		var inSpace, ok bool
		if inSpace, ok = existingKeys[portal.Key]; !ok || !inSpace {
			inSpace = user.addPortalToSpace(portal)
			if portal.IsPrivateChat() {
				puppet := user.bridge.GetPuppetByPID(portal.Key.PID)
				user.addPuppetToSpace(puppet)
			}
		}
		portalKeys = append(portalKeys, database.PortalKeyWithMeta{PortalKey: portal.Key, InCommunity: inSpace})
	}
	user.log.Infoln("Read chat list, updating user-portal mapping")
	err := user.SetPortalKeys(portalKeys)
	if err != nil {
		user.log.Warnln("Failed to update user-portal mapping:", err)

	}
	sort.Sort(chats)
	return chats
}

func (user *User) syncPortals(chatMap map[pulsesms.ChatID]pulsesms.Chat, createAll bool) {
	user.log.Infoln("syncing portals")
	// TODO use contexts instead of checking if user.Conn is the same?
	connAtStart := user.Pulse

	chats := user.collectChatList(chatMap)

	limit := user.bridge.Config.Bridge.InitialChatSync
	if limit < 0 {
		limit = len(chats)
	}
	if user.Pulse != connAtStart {
		user.log.Debugln("Connection seems to have changed before sync, cancelling")
		return
	}
	// now := time.Now().UTC().UnixNano() / 1e6
	// now := time.Now().Unix()
	user.log.Infoln("Syncing portals")
	doublePuppet := user.bridge.GetPuppetByCustomMXID(user.MXID)
	for i, chat := range chats {
		user.log.Debugfln("Syncing portal for %+v", chat.Chat.Name)

		// if chat.LastMessageTime+user.bridge.Config.Bridge.SyncChatMaxAge < now {
		// 	break
		// }
		// create := (chat.LastMessageTime >= user.LastConnection && user.LastConnection > 0) || i < limit
		create := i < limit
		if len(chat.Portal.MXID) > 0 || create || createAll {
			user.log.Debugfln("Syncing chat %+v", chat.Chat.Name)
			justCreated := len(chat.Portal.MXID) == 0
			user.syncPortal(chat)
			user.syncChatDoublePuppetDetails(doublePuppet, chat, justCreated)
		}
	}

	if user.Pulse != connAtStart {
		user.log.Debugln("Connection seems to have changed during sync, cancelling")
		return
	}
	user.UpdateDirectChats(nil)

	user.log.Infoln("Finished syncing portals")
	select {
	case user.syncPortalsDone <- struct{}{}:
	default:
	}
}

func (user *User) getDirectChats() map[id.UserID][]id.RoomID {
	res := make(map[id.UserID][]id.RoomID)
	privateChats := user.bridge.DB.Portal.FindPrivateChats(user.PID)
	for _, portal := range privateChats {
		if len(portal.MXID) > 0 {
			res[user.bridge.FormatPuppetMXID(portal.Key.PID)] = []id.RoomID{portal.MXID}
		}
	}
	return res
}

func (user *User) UpdateDirectChats(chats map[id.UserID][]id.RoomID) {
	user.bridge.Log.Info("updating direct chats")
	if !user.bridge.Config.Bridge.SyncDirectChatList {
		user.bridge.Log.Debugfln("syncDirectChatList disabled in config skipping")
		return
	}
	puppet := user.bridge.GetPuppetByCustomMXID(user.MXID)
	if puppet == nil || puppet.CustomIntent() == nil {
		user.bridge.Log.Debugfln("puppet or puppet intent nil, skipping")
		return
	}
	intent := puppet.CustomIntent()
	method := http.MethodPatch
	if chats == nil {
		chats = user.getDirectChats()
		method = http.MethodPut
	}
	user.log.Debugln("Updating m.direct list on homeserver")
	var err error
	if user.bridge.Config.Homeserver.Asmux {
		urlPath := intent.BuildBaseURL("_matrix", "client", "unstable", "com.beeper.asmux", "dms")
		_, err = intent.MakeFullRequest(mautrix.FullRequest{
			Method:      method,
			URL:         urlPath,
			Headers:     http.Header{"X-Asmux-Auth": {user.bridge.AS.Registration.AppToken}},
			RequestJSON: chats,
		})
	} else {
		existingChats := make(map[id.UserID][]id.RoomID)
		err = intent.GetAccountData(event.AccountDataDirectChats.Type, &existingChats)
		if err != nil {
			user.log.Warnln("Failed to get m.direct list to update it:", err)
			return
		}
		for userID, rooms := range existingChats {
			if _, ok := user.bridge.ParsePuppetMXID(userID); !ok {
				// This is not a ghost user, include it in the new list
				chats[userID] = rooms
			} else if _, ok := chats[userID]; !ok && method == http.MethodPatch {
				// This is a ghost user, but we're not replacing the whole list, so include it too
				chats[userID] = rooms
			}
		}
		err = intent.SetAccountData(event.AccountDataDirectChats.Type, &chats)
	}
	if err != nil {
		user.log.Warnln("Failed to update m.direct list:", err)
	}
}

func (user *User) HandleContactList(contacts []pulsesms.Contact) {
	user.log.Infoln("handling contact list")
	contactMap := make(map[pulsesms.PhoneNumber]pulsesms.Contact)
	for _, contact := range contacts {
		contactMap[contact.PhoneNumber] = contact
	}
	go user.syncPuppets(contactMap)
}

func (user *User) syncPuppets(contacts map[pulsesms.PhoneNumber]pulsesms.Contact) {
	user.log.Infoln("syncing puppets")
	if contacts == nil {
		contacts = user.Pulse.Store.Contacts
	}

	_, hasSelf := contacts[user.PID]
	if !hasSelf {
		contacts[user.PID] = pulsesms.Contact{
			Name: user.pushName,
			// Notify: user.pushName,
			PhoneNumber: user.PID,
		}
	}

	user.log.Infoln("Syncing puppet info from contacts")
	for _, contact := range contacts {
		puppet := user.bridge.GetPuppetByPID(contact.PhoneNumber)
		puppet.Sync(user, contact)

		// if strings.HasSuffix(pid, whatsapp.NewUserSuffix) {
		// 	puppet := user.bridge.GetPuppetByPID(contact.PhoneNumber)
		// 	puppet.Sync(user, contact)
		// } else if strings.HasSuffix(pid, whatsapp.BroadcastSuffix) {
		// 	portal := user.GetPortalByPID(contact.PhoneNumber)
		// 	portal.Sync(user, contact)
		// }
	}
	user.log.Infoln("Finished syncing puppet info from contacts")
}

func (user *User) updateLastConnectionIfNecessary() {
	if user.LastConnection+60 < time.Now().Unix() {
		user.UpdateLastConnection()
	}
}

func (user *User) HandleError(err error) {
	user.Pulse.Disconnect()
	go user.tryReconnect(fmt.Sprintf("Your PulseSMS connection failed: %v", err))
	// if !errors.Is(err, whatsapp.ErrInvalidWsData) {
	// 	user.log.Errorfln("WhatsApp error: %v", err)
	// }
	// if closed, ok := err.(*whatsapp.ErrConnectionClosed); ok {
	// 	user.bridge.Metrics.TrackDisconnection(user.MXID)
	// 	if closed.Code == 1000 && user.cleanDisconnection {
	// 		user.cleanDisconnection = false
	// 		if !user.bridge.Config.Bridge.AggressiveReconnect {
	// 			user.sendBridgeStatus(AsmuxPong{Error: AsmuxWANotConnected})
	// 			user.bridge.Metrics.TrackConnectionState(user.PID, false)
	// 			user.log.Infoln("Clean disconnection by server")
	// 			return
	// 		} else {
	// 			user.log.Debugln("Clean disconnection by server, but aggressive reconnection is enabled")
	// 		}
	// 	}
	// 	go user.tryReconnect(fmt.Sprintf("Your WhatsApp connection was closed with websocket status code %d", closed.Code))
	// } else if failed, ok := err.(*whatsapp.ErrConnectionFailed); ok {
	// 	user.Pulse.Disconnect()
	// 	user.bridge.Metrics.TrackDisconnection(user.MXID)
	// 	user.ConnectionErrors++
	// 	go user.tryReconnect(fmt.Sprintf("Your WhatsApp connection failed: %v", failed.Err))
	// } else if err == whatsapp.ErrPingFalse {
	// 	disconnectErr := user.Conn.Disconnect()
	// 	if disconnectErr != nil {
	// 		user.log.Warnln("Failed to disconnect after failed ping:", disconnectErr)
	// 	}
	// 	user.bridge.Metrics.TrackDisconnection(user.MXID)
	// 	user.ConnectionErrors++
	// 	go user.tryReconnect(fmt.Sprintf("Your WhatsApp connection failed: %v", err))
	// }
	// Otherwise unknown error, probably mostly harmless
}

func (user *User) tryReconnect(msg string) {
	// TODO: handle reconnect tries, look at upstream for whatsapp implementation
	user.Connect(false)

}

func (user *User) PortalKey(pid pulsesms.ChatID) database.PortalKey {
	return database.NewPortalKey(pid, user.PID)
}

func (user *User) GetPortalByPID(pid pulsesms.ChatID) *Portal {
	return user.bridge.GetPortalByPID(user.PortalKey(pid))
}

func (user *User) runMessageRingBuffer() {
	for msg := range user.messageInput {
		select {
		case user.messageOutput <- msg:
			user.bridge.Metrics.TrackBufferLength(user.MXID, len(user.messageOutput))
		default:
			dropped := <-user.messageOutput
			user.log.Warnln("Buffer is full, dropping message in", dropped.chat)
			user.messageOutput <- msg
		}
	}
}

func (user *User) handleMessageLoop() {
	for {
		select {
		case msg := <-user.messageOutput:
			user.bridge.Metrics.TrackBufferLength(user.MXID, len(user.messageOutput))
			user.GetPortalByPID(msg.chat).messages <- msg
		case <-user.syncStart:
			user.log.Debugln("Processing of incoming messages is locked")
			user.bridge.Metrics.TrackSyncLock(user.PID, true)
			user.syncWait.Wait()
			user.bridge.Metrics.TrackSyncLock(user.PID, false)
			user.log.Debugln("Processing of incoming messages unlocked")
		}
	}
}

// TODO
func (user *User) HandleNewContact(contact pulsesms.Contact) {
	user.log.Debugfln("Contact message: %+v", contact)
}

// func (user *User) HandleMsgInfo(info whatsapp.JSONMsgInfo) {
func (user *User) HandleMsgInfo(info pulsesms.Message) {
	// TODO: add command to message or more likely create message wrapper
	return
	// if (info.Command == whatsapp.MsgInfoCommandAck || info.Command == whatsapp.MsgInfoCommandAcks) && info.Acknowledgement == whatsapp.AckMessageRead {
	// 	portal := user.GetPortalByPID(info.ToPID)
	// 	if len(portal.MXID) == 0 {
	// 		return
	// 	}

	// 	intent := user.bridge.GetPuppetByPID(info.SenderPID).IntentFor(portal)
	// 	for _, msgID := range info.IDs {
	// 		msg := user.bridge.DB.Message.GetByPID(portal.Key, msgID)
	// 		if msg == nil || msg.IsFakeMXID() {
	// 			continue
	// 		}

	// 		err := intent.MarkReadWithContent(portal.MXID, msg.MXID, &CustomReadReceipt{DoublePuppet: intent.IsCustomPuppet})
	// 		if err != nil {
	// 			user.log.Warnln("Failed to mark message %s as read by %s: %v", msg.MXID, info.SenderPID, err)
	// 		}
	// 	}
	// }
}

// func (user *User) HandleReceivedMessage(received whatsapp.ReceivedMessage) {
// 	if received.Type == "read" {
// 		go user.markSelfRead(received.pid, received.Index)
// 	} else {
// 		user.log.Debugfln("Unknown received message type: %+v", received)
// 	}
// }

// func (user *User) HandleReadMessage(read whatsapp.ReadMessage) {
// 	user.log.Debugfln("Received chat read message: %+v", read)
// 	go user.markSelfRead(read.pid, "")
// }

// func (user *User) markSelfRead(pid, messageID string) {
// 	if strings.HasSuffix(pid, whatsapp.OldUserSuffix) {
// 		pid = strings.Replace(pid, whatsapp.OldUserSuffix, whatsapp.NewUserSuffix, -1)
// 	}
// 	puppet := user.bridge.GetPuppetByPID(user.PID)
// 	if puppet == nil {
// 		return
// 	}
// 	intent := puppet.CustomIntent()
// 	if intent == nil {
// 		return
// 	}
// 	portal := user.GetPortalByPID(pid)
// 	if portal == nil {
// 		return
// 	}
// 	var message *database.Message
// 	if messageID == "" {
// 		message = user.bridge.DB.Message.GetLastInChat(portal.Key)
// 		if message == nil {
// 			return
// 		}
// 		user.log.Debugfln("User read chat %s/%s in WhatsApp mobile (last known event: %s/%s)", portal.Key.PID, portal.MXID, message.PID, message.MXID)
// 	} else {
// 		message = user.bridge.DB.Message.GetByPID(portal.Key, messageID)
// 		if message == nil || message.IsFakeMXID() {
// 			return
// 		}
// 		user.log.Debugfln("User read message %s/%s in %s/%s in WhatsApp mobile", message.PID, message.MXID, portal.Key.PID, portal.MXID)
// 	}
// 	err := intent.MarkReadWithContent(portal.MXID, message.MXID, &CustomReadReceipt{DoublePuppet: true})
// 	if err != nil {
// 		user.log.Warnfln("Failed to bridge own read receipt in %s: %v", pid, err)
// 	}
// }

// TODO: pulse conversation update
// func (user *User) HandleChatUpdate(cmd whatsapp.ChatUpdate) {
// 	if cmd.Command != whatsapp.ChatUpdateCommandAction {
// 		return
// 	}

// 	portal := user.GetPortalByPID(cmd.PID)
// 	if len(portal.MXID) == 0 {
// 		if cmd.Data.Action == whatsapp.ChatActionIntroduce || cmd.Data.Action == whatsapp.ChatActionCreate {
// 			go func() {
// 				err := portal.CreateMatrixRoom(user)
// 				if err != nil {
// 					user.log.Errorln("Failed to create portal room after receiving join event:", err)
// 				}
// 			}()
// 		}
// 		return
// 	}

// 	// These don't come down the message history :(
// 	switch cmd.Data.Action {
// 	case whatsapp.ChatActionAddTopic:
// 		go portal.UpdateTopic(cmd.Data.AddTopic.Topic, cmd.Data.SenderPID, nil, true)
// 	case whatsapp.ChatActionRemoveTopic:
// 		go portal.UpdateTopic("", cmd.Data.SenderPID, nil, true)
// 	case whatsapp.ChatActionRemove:
// 		// We ignore leaving groups in the message history to avoid accidentally leaving rejoined groups,
// 		// but if we get a real-time command that says we left, it should be safe to bridge it.
// 		if !user.bridge.Config.Bridge.ChatMetaSync {
// 			for _, pid := range cmd.Data.UserChange.PIDs {
// 				if pid == user.PID {
// 					go portal.HandleWhatsAppKick(nil, cmd.Data.SenderPID, cmd.Data.UserChange.PIDs)
// 					break
// 				}
// 			}
// 		}
// 	}

// 	if !user.bridge.Config.Bridge.ChatMetaSync {
// 		// Ignore chat update commands, we're relying on the message history.
// 		return
// 	}

// 	switch cmd.Data.Action {
// 	case whatsapp.ChatActionNameChange:
// 		go portal.UpdateName(cmd.Data.NameChange.Name, cmd.Data.SenderPID, nil, true)
// 	case whatsapp.ChatActionPromote:
// 		go portal.ChangeAdminStatus(cmd.Data.UserChange.PIDs, true)
// 	case whatsapp.ChatActionDemote:
// 		go portal.ChangeAdminStatus(cmd.Data.UserChange.PIDs, false)
// 	case whatsapp.ChatActionAnnounce:
// 		go portal.RestrictMessageSending(cmd.Data.Announce)
// 	case whatsapp.ChatActionRestrict:
// 		go portal.RestrictMetadataChanges(cmd.Data.Restrict)
// 	case whatsapp.ChatActionRemove:
// 		go portal.HandleWhatsAppKick(nil, cmd.Data.SenderPID, cmd.Data.UserChange.PIDs)
// 	case whatsapp.ChatActionAdd:
// 		go portal.HandleWhatsAppInvite(cmd.Data.SenderPID, nil, cmd.Data.UserChange.PIDs)
// 	case whatsapp.ChatActionIntroduce:
// 		if cmd.Data.SenderPID != "unknown" {
// 			go portal.Sync(user, whatsapp.Contact{PID: portal.Key.PID})
// 		}
// 	}
// }

func (user *User) NeedsRelaybot(portal *Portal) bool {
	return !user.HasSession() || !user.IsInPortal(portal.Key)
}
