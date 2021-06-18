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
	"fmt"
	"strings"
	"time"

	"github.com/treethought/pulsesms"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
)

type UserQuery struct {
	db  *Database
	log log.Logger
}

func (uq *UserQuery) New() *User {
	return &User{
		db:  uq.db,
		log: uq.log,
	}
}

func (uq *UserQuery) GetAll() (users []*User) {
	rows, err := uq.db.Query(`SELECT mxid, pid, management_room, last_connection, account_id, password_hash, salt FROM "user"`)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		users = append(users, uq.New().Scan(rows))
	}
	return
}

func (uq *UserQuery) GetByMXID(userID id.UserID) *User {
	row := uq.db.QueryRow(`SELECT mxid, pid, management_room, last_connection, account_id, password_hash, salt FROM "user" WHERE mxid=$1`, userID)
	if row == nil {
		return nil
	}
	return uq.New().Scan(row)
}

func (uq *UserQuery) GetByPID(userID pulsesms.PhoneNumber) *User {
	uq.log.Warnfln("getting db user by pid %s", userID)
	row := uq.db.QueryRow(`SELECT mxid, pid, management_room, last_connection, account_id, password_hash, salt FROM "user" WHERE pid=$1`, (userID))
	if row == nil {
		return nil
	}
	return uq.New().Scan(row)
}

type User struct {
	db             *Database
	log            log.Logger
	MXID           id.UserID
	PID            pulsesms.PhoneNumber
	ManagementRoom id.RoomID
	Session        *pulsesms.KeyCredentials
	LastConnection int64
	AccountID      pulsesms.AccountID
}

func (user *User) Scan(row Scannable) *User {
	var pid, accountID, pwHash, salt sql.NullString
	// var encKey, macKey []byte
	// err := row.Scan(&user.MXID, &pid, &user.ManagementRoom, &user.LastConnection, &accountID, &pwHash, &salt, &encKey, &macKey)
	err := row.Scan(&user.MXID, &pid, &user.ManagementRoom, &user.LastConnection, &accountID, &pwHash, &salt)
	if err != nil {
		if err != sql.ErrNoRows {
			user.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	if len(accountID.String) > 0 {
		user.Session = &pulsesms.KeyCredentials{
			AccountID:    pulsesms.AccountID(accountID.String),
			PasswordHash: pwHash.String,
			Salt:         salt.String,
		}

	} else {
		user.Session = nil
	}

	if len(pid.String) > 0 {
		user.PID = pid.String
	}
	return user
}

// func stripSuffix(pid pulsesms.ConversationID) string {
// 	if len(pid) == 0 {
// 		return pid
// 	}

// 	index := strings.IndexRune(pid, '@')
// 	if index < 0 {
// 		return pid
// 	}

// 	return pid[:index]
// }

func (user *User) pidPtr() *string {
	str := fmt.Sprint(user.PID)
	return &str
	// return string(user.PID)
	// if user.PID != 0 {
	// str := stripSuffix(user.PID)
	// return &str
	// }
	// return nil
}

func (user *User) sessionUnptr() (sess pulsesms.KeyCredentials) {
	if user.Session != nil {
		sess = *user.Session
	}
	return
}

func (user *User) Insert() {
	sess := user.sessionUnptr()
	_, err := user.db.Exec(`INSERT INTO "user" (mxid, pid, management_room, last_connection, account_id, password_hash, salt) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		user.MXID, user.pidPtr(),
		user.ManagementRoom, user.LastConnection,
		sess.AccountID, sess.PasswordHash, sess.Salt)
	if err != nil {
		user.log.Warnfln("Failed to insert %s: %v", user.MXID, err)
	}
}

func (user *User) UpdateLastConnection() {
	user.LastConnection = time.Now().Unix()
	_, err := user.db.Exec(`UPDATE "user" SET last_connection=$1 WHERE mxid=$2`,
		user.LastConnection, user.MXID)
	if err != nil {
		user.log.Warnfln("Failed to update last connection ts: %v", err)
	}
}

func (user *User) Update() {
	sess := user.sessionUnptr()
	_, err := user.db.Exec(`UPDATE "user" SET pid=$1, management_room=$2, last_connection=$3, account_id=$4, password_hash=$5, salt= $6 WHERE mxid=$7`,
		user.pidPtr(), user.ManagementRoom, user.LastConnection,
		sess.AccountID, sess.PasswordHash, sess.Salt,
		user.MXID)
	if err != nil {
		user.log.Warnfln("Failed to update %s: %v", user.MXID, err)
	}
}

type PortalKeyWithMeta struct {
	PortalKey
	InCommunity bool
}

func (user *User) SetPortalKeys(newKeys []PortalKeyWithMeta) error {
	tx, err := user.db.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM user_portal WHERE user_pid=$1", user.pidPtr())
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	valueStrings := make([]string, len(newKeys))
	values := make([]interface{}, len(newKeys)*4)
	for i, key := range newKeys {
		pos := i * 4
		valueStrings[i] = fmt.Sprintf("($%d, $%d, $%d, $%d)", pos+1, pos+2, pos+3, pos+4)
		values[pos] = user.pidPtr()
		values[pos+1] = key.PID
		values[pos+2] = key.Receiver
		values[pos+3] = key.InCommunity
	}
	query := fmt.Sprintf("INSERT INTO user_portal (user_pid, portal_pid, portal_receiver, in_community) VALUES %s",
		strings.Join(valueStrings, ", "))
	_, err = tx.Exec(query, values...)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (user *User) IsInPortal(key PortalKey) bool {
	row := user.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM user_portal WHERE user_pid=$1 AND portal_pid=$2 AND portal_receiver=$3)`, user.pidPtr(), &key.PID, &key.Receiver)
	var exists bool
	_ = row.Scan(&exists)
	return exists
}

func (user *User) GetPortalKeys() []PortalKey {
	rows, err := user.db.Query(`SELECT portal_pid, portal_receiver FROM user_portal WHERE user_pid=$1`, user.pidPtr())
	if err != nil {
		user.log.Warnln("Failed to get user portal keys:", err)
		return nil
	}
	var keys []PortalKey
	for rows.Next() {
		var key PortalKey
		err = rows.Scan(&key.PID, &key.Receiver)
		if err != nil {
			user.log.Warnln("Failed to scan row:", err)
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func (user *User) GetInCommunityMap() map[PortalKey]bool {
	rows, err := user.db.Query(`SELECT portal_pid, portal_receiver, in_community FROM user_portal WHERE user_pid=$1`, user.pidPtr())
	if err != nil {
		user.log.Warnln("Failed to get user portal keys:", err)
		return nil
	}
	keys := make(map[PortalKey]bool)
	for rows.Next() {
		var key PortalKey
		var inCommunity bool
		err = rows.Scan(&key.PID, &key.Receiver, &inCommunity)
		if err != nil {
			user.log.Warnln("Failed to scan row:", err)
			continue
		}
		keys[key] = inCommunity
	}
	return keys
}

func (user *User) CreateUserPortal(newKey PortalKeyWithMeta) {
	user.log.Debugfln("Creating new portal %s for receiver: %s", newKey.PortalKey.PID, newKey.PortalKey.Receiver)
	_, err := user.db.Exec(`INSERT INTO user_portal (user_pid, portal_pid, portal_receiver, in_community) VALUES ($1, $2, $3, $4)`,
		user.pidPtr(),
		newKey.PortalKey.PID, newKey.PortalKey.Receiver,
		newKey.InCommunity)
	if err != nil {
		user.log.Warnfln("Failed to insert %s: %v", user.MXID, err)
	}
}
