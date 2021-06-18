// matrix-pulsesms - A Matrix-PulseSMS puppeting bridge.
// Copyright (C) 2021 Cam Sweeney
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
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
)

func (user *User) inviteToSpace() {
	_, err := user.bridge.Bot.InviteUser(user.SpaceID, &mautrix.ReqInviteUser{UserID: user.MXID})
	if err != nil {
		user.log.Warnln("Failed to invite user to space:", err)
	}
}

func (user *User) updateSpaceProfile() {
	return
	// user.bridge.Bot.SetRoomName()
	// url := user.bridge.Bot.BuildURL("groups", user.CommunityID, "profile")
	// profileReq := struct {
	// Name             string `json:"name"`
	// AvatarURL        string `json:"avatar_url"`
	// ShortDescription string `json:"short_description"`
	// }{"WhatsApp", user.bridge.Config.AppService.Bot.Avatar, "Your WhatsApp bridged chats"}
	// _, err := user.bridge.Bot.MakeRequest(http.MethodPost, url, &profileReq, nil)
	// if err != nil {
	// user.log.Warnfln("Failed to update metadata of %s: %v", user.CommunityID, err)
	// }
}

func (user *User) createSpace() {
	if user.IsRelaybot || !user.bridge.Config.Bridge.EnableSpaces() {
		return
	}

	localpart, server, _ := user.MXID.Parse()
	space := user.bridge.Config.Bridge.FormatSpace(localpart, server)
	user.log.Debugln("Creating personal filtering space", space)
	bot := user.bridge.Bot
	req := struct {
		Localpart string `json:"localpart"`
	}{space}
	// resp := struct {
	// 	GroupID string `json:"group_id"`
	// }{}

	resp, err := bot.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility: "private",
		Name:       space,
		// Topic:        portal.Topic,
		// Invite:       invite,
		// Preset:       "private_chat",
		IsDirect: false,
		CreationContent: map[string]interface{}{
			"type": "m.space",
		},
	})
	if err != nil {
		user.log.Errorfln("failed to create space %v", err)
		if httpErr, ok := err.(mautrix.HTTPError); ok {
			if httpErr.RespError.Err != "Group already exists" {
				user.log.Warnln("Server responded with error creating personal filtering space:", err)
				return
			} else {
				user.log.Debugln("Personal filtering space", resp.RoomID.String(), "already existed")
				user.CommunityID = fmt.Sprintf("+%s:%s", req.Localpart, user.bridge.Config.Homeserver.Domain)
			}
		} else {
			user.log.Warnln("Unknown error creating personal filtering community:", err)
			return
		}
	} else {
		user.log.Infoln("Created personal filtering community %s", resp.RoomID)
		user.SpaceID = resp.RoomID
		user.inviteToSpace()
		// user.updateCommunityProfile()
	}
}

func (user *User) addPuppetToSpace(puppet *Puppet) bool {
	if user.IsRelaybot || len(user.SpaceID) == 0 {
		return false
	}
	bot := user.bridge.Bot
	url := bot.BuildURL("groups", user.CommunityID, "admin", "users", "invite", puppet.MXID)
	blankReqBody := map[string]interface{}{}
	_, err := bot.MakeRequest(http.MethodPut, url, &blankReqBody, nil)
	if err != nil {
		user.log.Warnfln("Failed to invite %s to %s: %v", puppet.MXID, user.CommunityID, err)
		return false
	}
	reqBody := map[string]map[string]string{
		"m.visibility": {
			"type": "private",
		},
	}
	url = bot.BuildURLWithQuery(mautrix.URLPath{"groups", user.CommunityID, "self", "accept_invite"}, map[string]string{
		"user_id": puppet.MXID.String(),
	})
	_, err = bot.MakeRequest(http.MethodPut, url, &reqBody, nil)
	if err != nil {
		user.log.Warnfln("Failed to join %s as %s: %v", user.CommunityID, puppet.MXID, err)
		return false
	}

	_, _ = user.bridge.Bot.InviteUser(user.SpaceID, &mautrix.ReqInviteUser{UserID: puppet.MXID})
	puppet.DefaultIntent().EnsureJoined(user.SpaceID)
	user.log.Debugln("Added", puppet.MXID, "to", user.CommunityID)
	return true
}

type spaceEventContent struct {
	Via       []string `json:"via,omitempty"`
	Order     string   `json:"order,omitempty"`
	Canonical bool     `json:"canonical,omitempty"`
}

func (user *User) addPortalToSpace(portal *Portal) bool {

	if user.IsRelaybot || len(user.SpaceID) == 0 || len(portal.MXID) == 0 {
		return false
	}
	bot := user.bridge.Bot

	// https://github.com/matrix-org/matrix-doc/blob/55c2866ef3a25adfcd1139fd23f603f8123c08d0/proposals/1772-groups-as-rooms.md#relationship-between-rooms-and-spaces

	// add the portal as a child
	childContent := spaceEventContent{
		Via: []string{user.bridge.AS.HomeserverDomain},
	}
	_, err := bot.SendStateEvent(user.SpaceID, event.Type{Type: "m.space.child"}, string(portal.MXID), childContent)
	if err != nil {
		user.log.Warnfln("Failed to add %s as child of space %s: %v", portal.MXID, user.SpaceID, err)
		return false
	}

	parentContent := spaceEventContent{
		Via:       []string{user.bridge.AS.HomeserverDomain},
		Canonical: true,
	}

	// add the space as the portal's canonical parent
	_, err = bot.SendStateEvent(portal.MXID, event.Type{Type: "m.space.parent"}, string(user.SpaceID), parentContent)
	if err != nil {
		user.log.Warnfln("Failed to add %s as parent space of room %s: %v", user.SpaceID, portal.MXID, err)
		return false
	}
	user.log.Debugln("Added", portal.MXID, "to", user.SpaceID)
	return true
}
