package upgrades

import (
	"database/sql"
)

func init() {
	upgrades[0] = upgrade{"Initial schema", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS portal (
			pid      VARCHAR(255),
			receiver VARCHAR(255),
			mxid     VARCHAR(255) UNIQUE,

			name   VARCHAR(255) NOT NULL,
			topic  VARCHAR(255) NOT NULL,
			avatar VARCHAR(255) NOT NULL,

			PRIMARY KEY (pid, receiver)
		)`)
		if err != nil {
			return err
		}

		_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS puppet (
			pid          VARCHAR(255) PRIMARY KEY,
			avatar       VARCHAR(255),
			displayname  VARCHAR(255),
			name_quality SMALLINT
		)`)
		if err != nil {
			return err
		}

		_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS "user" (
			mxid VARCHAR(255) PRIMARY KEY,
			pid  VARCHAR(255) UNIQUE,

			management_room VARCHAR(255),

			account_id    VARCHAR(255),
			password_hash VARCHAR(255),
			salt          VARCHAR(255),
			enc_key      bytea,
			mac_key      bytea
		)`)
		if err != nil {
			return err
		}

		_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS message (
			chat_pid      VARCHAR(255),
			chat_receiver VARCHAR(255),
			pid           VARCHAR(255),
			mxid          VARCHAR(255) NOT NULL UNIQUE,
			sender        VARCHAR(255) NOT NULL,
			content       bytea        NOT NULL,

			PRIMARY KEY (chat_pid, chat_receiver, pid),
			FOREIGN KEY (chat_pid, chat_receiver) REFERENCES portal(pid, receiver) ON DELETE CASCADE
		)`)
		if err != nil {
			return err
		}

		return nil
	}}
}
