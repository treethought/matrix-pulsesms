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
	"fmt"
	"regexp"
	"sync"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"github.com/treethought/matrix-pulsesms/database"
	"github.com/treethought/pulsesms"
)

var userIDRegex *regexp.Regexp

func (bridge *Bridge) ParsePuppetMXID(mxid id.UserID) (pulsesms.PhoneNumber, bool) {
	if userIDRegex == nil {
		userIDRegex = regexp.MustCompile(fmt.Sprintf("^@%s:%s$",
			// bridge.Config.Bridge.FormatUsername("([0-9]+)"),
			bridge.Config.Bridge.FormatUsername("[^a-zA-Z0-9]+"),
			bridge.Config.Homeserver.Domain))
	}
	match := userIDRegex.FindStringSubmatch(string(mxid))
	if match == nil || len(match) != 2 {
		return "", false
	}

	// pid := pulsesms.ChatID(match[1] + whatsapp.NewUserSuffix)
	pid := pulsesms.ChatID(match[1])
	bridge.Log.Debugfln("parsed puppet mxid %s", pid)
	return pid, true
}

func (bridge *Bridge) GetPuppetByMXID(mxid id.UserID) *Puppet {
	pid, ok := bridge.ParsePuppetMXID(mxid)
	if !ok {
		return nil
	}

	return bridge.GetPuppetByPID(pid)
}

func (bridge *Bridge) GetPuppetByPID(pid pulsesms.PhoneNumber) *Puppet {
	bridge.Log.Debugfln("getting puppet by pid: %s", pid)
	bridge.puppetsLock.Lock()
	defer bridge.puppetsLock.Unlock()
	puppet, ok := bridge.puppets[pid]
	if !ok {

		bridge.Log.Infofln("puppet by pid %s does not exist, creating", pid)
		dbPuppet := bridge.DB.Puppet.Get(pid)
		if dbPuppet == nil {
			dbPuppet = bridge.DB.Puppet.New()
			dbPuppet.PID = pid
			dbPuppet.Insert()
		}
		puppet = bridge.NewPuppet(dbPuppet)
		bridge.puppets[puppet.PID] = puppet
		if len(puppet.CustomMXID) > 0 {
			bridge.puppetsByCustomMXID[puppet.CustomMXID] = puppet
		}
	}
	return puppet
}

func (bridge *Bridge) GetPuppetByCustomMXID(mxid id.UserID) *Puppet {
	bridge.puppetsLock.Lock()
	defer bridge.puppetsLock.Unlock()
	puppet, ok := bridge.puppetsByCustomMXID[mxid]
	if !ok {
		dbPuppet := bridge.DB.Puppet.GetByCustomMXID(mxid)
		if dbPuppet == nil {
			return nil
		}
		puppet = bridge.NewPuppet(dbPuppet)
		bridge.puppets[puppet.PID] = puppet
		bridge.puppetsByCustomMXID[puppet.CustomMXID] = puppet
	}
	return puppet
}

func (bridge *Bridge) GetAllPuppetsWithCustomMXID() []*Puppet {
	return bridge.dbPuppetsToPuppets(bridge.DB.Puppet.GetAllWithCustomMXID())
}

func (bridge *Bridge) GetAllPuppets() []*Puppet {
	return bridge.dbPuppetsToPuppets(bridge.DB.Puppet.GetAll())
}

func (bridge *Bridge) dbPuppetsToPuppets(dbPuppets []*database.Puppet) []*Puppet {
	bridge.puppetsLock.Lock()
	defer bridge.puppetsLock.Unlock()
	output := make([]*Puppet, len(dbPuppets))
	for index, dbPuppet := range dbPuppets {
		if dbPuppet == nil {
			continue
		}
		puppet, ok := bridge.puppets[dbPuppet.PID]
		if !ok {
			puppet = bridge.NewPuppet(dbPuppet)
			bridge.puppets[dbPuppet.PID] = puppet
			if len(dbPuppet.CustomMXID) > 0 {
				bridge.puppetsByCustomMXID[dbPuppet.CustomMXID] = puppet
			}
		}
		output[index] = puppet
	}
	return output
}

func (bridge *Bridge) FormatPuppetMXID(pid pulsesms.PhoneNumber) id.UserID {
	bridge.Log.Debugfln("Formatting puppet mxid for :%s", pid)
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		log.Fatal(err)
	}
	cleanID := reg.ReplaceAllString(pid, "")
	return id.NewUserID(
		bridge.Config.Bridge.FormatUsername(cleanID),
		bridge.Config.Homeserver.Domain)
}

func (bridge *Bridge) NewPuppet(dbPuppet *database.Puppet) *Puppet {
	return &Puppet{
		Puppet: dbPuppet,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Puppet/%s", dbPuppet.PID)),

		MXID: bridge.FormatPuppetMXID(dbPuppet.PID),
	}
}

type Puppet struct {
	*database.Puppet

	bridge *Bridge
	log    log.Logger

	typingIn id.RoomID
	typingAt int64

	MXID id.UserID

	customIntent   *appservice.IntentAPI
	customTypingIn map[id.RoomID]bool
	customUser     *User

	syncLock sync.Mutex
}

func (puppet *Puppet) PhoneNumber() string {
    return puppet.PID
}

func (puppet *Puppet) IntentFor(portal *Portal) *appservice.IntentAPI {
	if (!portal.IsPrivateChat() && puppet.customIntent == nil) ||
		(portal.backfilling && portal.bridge.Config.Bridge.InviteOwnPuppetForBackfilling) ||
		portal.Key.PID == puppet.PID {
		return puppet.DefaultIntent()
	}
	return puppet.customIntent
}

func (puppet *Puppet) CustomIntent() *appservice.IntentAPI {
	return puppet.customIntent
}

func (puppet *Puppet) DefaultIntent() *appservice.IntentAPI {
	return puppet.bridge.AS.Intent(puppet.MXID)
}

func (puppet *Puppet) UpdateAvatar(source *User, avatar *profilePicInfo) bool {
	return false
	// if avatar == nil {
	// 	var err error
	// 	avatar, err = source.Pulse.GetProfilePicThumb(puppet.PID)
	// 	if err != nil {
	// 		puppet.log.Warnln("Failed to get avatar:", err)
	// 		return false
	// 	}
	// }

	// if avatar.Status == 404 {
	// 	avatar.Tag = "remove"
	// 	avatar.Status = 0
	// } else if avatar.Status == 401 && puppet.Avatar != "unauthorized" {
	// 	puppet.Avatar = "unauthorized"
	// 	return true
	// }
	// if avatar.Status != 0 || avatar.Tag == puppet.Avatar {
	// 	return false
	// }

	// if avatar.Tag == "remove" || len(avatar.URL) == 0 {
	// 	err := puppet.DefaultIntent().SetAvatarURL(id.ContentURI{})
	// 	if err != nil {
	// 		puppet.log.Warnln("Failed to remove avatar:", err)
	// 	}
	// 	puppet.AvatarURL = id.ContentURI{}
	// 	puppet.Avatar = avatar.Tag
	// 	go puppet.updatePortalAvatar()
	// 	return true
	// }

	// data, err := avatar.DownloadBytes()
	// if err != nil {
	// 	puppet.log.Warnln("Failed to download avatar:", err)
	// 	return false
	// }

	// mime := http.DetectContentType(data)
	// resp, err := puppet.DefaultIntent().UploadBytes(data, mime)
	// if err != nil {
	// 	puppet.log.Warnln("Failed to upload avatar:", err)
	// 	return false
	// }

	// puppet.AvatarURL = resp.ContentURI
	// err = puppet.DefaultIntent().SetAvatarURL(puppet.AvatarURL)
	// if err != nil {
	// 	puppet.log.Warnln("Failed to set avatar:", err)
	// }
	// puppet.Avatar = avatar.Tag
	// go puppet.updatePortalAvatar()
	// return true
}

func (puppet *Puppet) UpdateName(source *User, contact pulsesms.Contact) bool {
	newName, quality := puppet.bridge.Config.Bridge.FormatDisplayname(contact)
	if puppet.Displayname != newName && quality >= puppet.NameQuality {
		err := puppet.DefaultIntent().SetDisplayName(newName)
		if err == nil {
			puppet.Displayname = newName
			puppet.NameQuality = quality
			go puppet.updatePortalName()
			puppet.Update()
		} else {
			puppet.log.Warnln("Failed to set display name:", err)
		}
		return true
	}
	return false
}

func (puppet *Puppet) updatePortalMeta(meta func(portal *Portal)) {
	if puppet.bridge.Config.Bridge.PrivateChatPortalMeta {
		for _, portal := range puppet.bridge.GetAllPortalsByPID(puppet.PID) {
			meta(portal)
		}
	}
}

func (puppet *Puppet) updatePortalAvatar() {
	puppet.updatePortalMeta(func(portal *Portal) {
		if len(portal.MXID) > 0 {
			_, err := portal.MainIntent().SetRoomAvatar(portal.MXID, puppet.AvatarURL)
			if err != nil {
				portal.log.Warnln("Failed to set avatar:", err)
			}
		}
		portal.AvatarURL = puppet.AvatarURL
		portal.Avatar = puppet.Avatar
		portal.Update()
	})
}

func (puppet *Puppet) updatePortalName() {
	puppet.updatePortalMeta(func(portal *Portal) {
		if len(portal.MXID) > 0 {
			_, err := portal.MainIntent().SetRoomName(portal.MXID, puppet.Displayname)
			if err != nil {
				portal.log.Warnln("Failed to set name:", err)
			}
		}
		portal.Name = puppet.Displayname
		portal.Update()
	})
}

func (puppet *Puppet) SyncContactIfNecessary(source *User) {
	if len(puppet.Displayname) > 0 {
		return
	}
	contact, ok := source.Pulse.Store.Contacts[puppet.PID]
	if !ok {
		puppet.log.Warnfln("No contact info found through %s in SyncContactIfNecessary", source.MXID)
		contact.PhoneNumber = puppet.PID
		// Sync anyway to set a phone number name
	} else {
		puppet.log.Debugfln("Syncing contact info through %s / %s because puppet has no displayname", source.MXID, source.PID)
	}
	puppet.Sync(source, contact)
}

func (puppet *Puppet) Sync(source *User, contact pulsesms.Contact) {
	puppet.log.Infofln("syncing puppet %s", contact.Name)
	puppet.syncLock.Lock()
	defer puppet.syncLock.Unlock()
	err := puppet.DefaultIntent().EnsureRegistered()
	if err != nil {
		puppet.log.Errorln("Failed to ensure registered:", err)
	}

	// if contact.PID == source.PID {
	// 	contact.Notify = source.pushName
	// }

	update := false
	update = puppet.UpdateName(source, contact) || update
	// TODO figure out how to update avatars after being offline
	if len(puppet.Avatar) == 0 || puppet.bridge.Config.Bridge.UserAvatarSync {
		update = puppet.UpdateAvatar(source, nil) || update
	}
	if update {
		puppet.Update()
	}
}
