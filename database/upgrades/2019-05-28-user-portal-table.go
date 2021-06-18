package upgrades

import (
	"database/sql"
)

func init() {
	upgrades[6] = upgrade{"Add user-portal mapping table", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec(`CREATE TABLE user_portal (
			user_pid        VARCHAR(255),
			portal_pid      VARCHAR(255),
			portal_receiver VARCHAR(255),
			PRIMARY KEY (user_pid, portal_pid, portal_receiver),
			FOREIGN KEY (user_pid)                    REFERENCES "user"(pid)           ON DELETE CASCADE,
			FOREIGN KEY (portal_pid, portal_receiver) REFERENCES portal(pid, receiver) ON DELETE CASCADE
		)`)
		return err
	}}
}
