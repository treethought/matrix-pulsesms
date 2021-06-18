// mautrix-pulsesms - A Matrix-WhatsApp puppeting bridge.
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

package database

import (
	"database/sql"

	log "maunium.net/go/maulogger/v2"

	"github.com/treethought/pulsesms"

	"maunium.net/go/mautrix/id"
)

type PortalKey struct {
	PID      pulsesms.ChatID
	Receiver pulsesms.ChatID
}

func GroupPortalKey(pid pulsesms.ChatID) PortalKey {
	return PortalKey{
		PID:      pid,
		Receiver: pid,
	}
}

func NewPortalKey(pid, receiver pulsesms.ChatID) PortalKey {
	log.Infofln("creating portal key with pid: %s, receiver: %s", pid, receiver)

	// if strings.HasSuffix(pid, whatsapp.GroupSuffix) {
	// 	receiver = pid
	// }
	return PortalKey{
		PID:      pid,
		Receiver: receiver,
	}
}

func (key PortalKey) String() string {
	if key.Receiver == key.PID {
		return key.PID
	}
	return key.PID + "-" + key.Receiver
}

type PortalQuery struct {
	db  *Database
	log log.Logger
}

func (pq *PortalQuery) New() *Portal {
	return &Portal{
		db:  pq.db,
		log: pq.log,
	}
}

func (pq *PortalQuery) GetAll() []*Portal {
	return pq.getAll("SELECT * FROM portal")
}

func (pq *PortalQuery) GetByPID(key PortalKey) *Portal {
	return pq.get("SELECT * FROM portal WHERE pid=$1 AND receiver=$2", key.PID, key.Receiver)
}

func (pq *PortalQuery) GetByMXID(mxid id.RoomID) *Portal {
	return pq.get("SELECT * FROM portal WHERE mxid=$1", mxid)
}

func (pq *PortalQuery) GetAllByPID(pid pulsesms.PhoneNumber) []*Portal {
	return pq.getAll("SELECT * FROM portal WHERE pid=$1", pid)
}

func (pq *PortalQuery) FindPrivateChats(receiver pulsesms.PhoneNumber) []*Portal {
	return pq.getAll("SELECT * FROM portal WHERE receiver=$1 AND pid LIKE '%@s.whatsapp.net'", receiver)
}

func (pq *PortalQuery) getAll(query string, args ...interface{}) (portals []*Portal) {
	rows, err := pq.db.Query(query, args...)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		portals = append(portals, pq.New().Scan(rows))
	}
	return
}

func (pq *PortalQuery) get(query string, args ...interface{}) *Portal {
	row := pq.db.QueryRow(query, args...)
	if row == nil {
		return nil
	}
	return pq.New().Scan(row)
}

type Portal struct {
	db  *Database
	log log.Logger

	Key  PortalKey
	MXID id.RoomID

	Name      string
	Topic     string
	Avatar    string
	AvatarURL id.ContentURI
	Encrypted bool
}

func (portal *Portal) Scan(row Scannable) *Portal {
	var mxid, avatarURL sql.NullString
	err := row.Scan(&portal.Key.PID, &portal.Key.Receiver, &mxid, &portal.Name, &portal.Topic, &portal.Avatar, &avatarURL, &portal.Encrypted)
	if err != nil {
		if err != sql.ErrNoRows {
			portal.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	portal.MXID = id.RoomID(mxid.String)
	portal.AvatarURL, _ = id.ParseContentURI(avatarURL.String)
	return portal
}

func (portal *Portal) mxidPtr() *id.RoomID {
	if len(portal.MXID) > 0 {
		return &portal.MXID
	}
	return nil
}

func (portal *Portal) Insert() {
	_, err := portal.db.Exec("INSERT INTO portal (pid, receiver, mxid, name, topic, avatar, avatar_url, encrypted) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		portal.Key.PID, portal.Key.Receiver, portal.mxidPtr(), portal.Name, portal.Topic, portal.Avatar, portal.AvatarURL.String(), portal.Encrypted)
	if err != nil {
		portal.log.Warnfln("Failed to insert %s: %v", portal.Key, err)
	}
}

func (portal *Portal) Update() {
	var mxid *id.RoomID
	if len(portal.MXID) > 0 {
		mxid = &portal.MXID
	}
	_, err := portal.db.Exec("UPDATE portal SET mxid=$1, name=$2, topic=$3, avatar=$4, avatar_url=$5, encrypted=$6 WHERE pid=$7 AND receiver=$8",
		mxid, portal.Name, portal.Topic, portal.Avatar, portal.AvatarURL.String(), portal.Encrypted, portal.Key.PID, portal.Key.Receiver)
	if err != nil {
		portal.log.Warnfln("Failed to update %s: %v", portal.Key, err)
	}
}

func (portal *Portal) Delete() {
	_, err := portal.db.Exec("DELETE FROM portal WHERE pid=$1 AND receiver=$2", portal.Key.PID, portal.Key.Receiver)
	if err != nil {
		portal.log.Warnfln("Failed to delete %s: %v", portal.Key, err)
	}
}

func (portal *Portal) GetUserIDs() []id.UserID {
	rows, err := portal.db.Query(`SELECT "user".mxid FROM "user", user_portal
		WHERE "user".pid=user_portal.user_pid
			AND user_portal.portal_pid=$1
			AND user_portal.portal_receiver=$2`,
		portal.Key.PID, portal.Key.Receiver)
	if err != nil {
		portal.log.Debugln("Failed to get portal user ids:", err)
		return nil
	}
	var userIDs []id.UserID
	for rows.Next() {
		var userID id.UserID
		err = rows.Scan(&userID)
		if err != nil {
			portal.log.Warnln("Failed to scan row:", err)
			continue
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs
}
