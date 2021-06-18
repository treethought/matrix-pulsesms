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
	"net/http"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix/id"
)

type AsmuxError string

const (
	AsmuxWANotLoggedIn  AsmuxError = "wa-not-logged-in"
	AsmuxWANotConnected AsmuxError = "wa-not-connected"
	AsmuxWAConnecting   AsmuxError = "wa-connecting"
	AsmuxWATimeout      AsmuxError = "wa-timeout"
	AsmuxWAPingFalse    AsmuxError = "wa-ping-false"
	AsmuxWAPingError    AsmuxError = "wa-ping-error"
)

var asmuxHumanErrors = map[AsmuxError]string{
	AsmuxWANotLoggedIn:  "You're not logged into WhatsApp",
	AsmuxWANotConnected: "You're not connected to WhatsApp",
	AsmuxWAConnecting:   "Trying to reconnect to WhatsApp. Please make sure WhatsApp is running on your phone and connected to the internet.",
	AsmuxWATimeout:      "WhatsApp on your phone is not responding. Please make sure it is running and connected to the internet.",
	AsmuxWAPingFalse:    "WhatsApp returned an error, reconnecting. Please make sure WhatsApp is running on your phone and connected to the internet.",
	AsmuxWAPingError:    "WhatsApp returned an unknown error",
}

type AsmuxPong struct {
	OK          bool       `json:"ok"`
	Timestamp   int64      `json:"timestamp"`
	TTL         int        `json:"ttl"`
	ErrorSource string     `json:"error_source,omitempty"`
	Error       AsmuxError `json:"error,omitempty"`
	Message     string     `json:"message,omitempty"`
}

func (pong *AsmuxPong) fill() {
	pong.Timestamp = time.Now().Unix()
	if !pong.OK {
		pong.TTL = 60
		pong.ErrorSource = "bridge"
		pong.Message = asmuxHumanErrors[pong.Error]
	} else {
		pong.TTL = 240
	}
}

func (pong *AsmuxPong) shouldDeduplicate(newPong *AsmuxPong) bool {
	if pong == nil || pong.OK != newPong.OK || pong.Error != newPong.Error {
		return false
	}
	return pong.Timestamp+int64(pong.TTL/5) > time.Now().Unix()
}

// TODO
func (user *User) setupAdminTestHooks() {
	if !user.bridge.Config.Homeserver.Asmux {
		return
	}
}

func (user *User) sendBridgeStatus(state AsmuxPong) {
	if !user.bridge.Config.Homeserver.Asmux {
		return
	}
	state.fill()
	if user.prevBridgeStatus != nil && user.prevBridgeStatus.shouldDeduplicate(&state) {
		return
	}
	cli := user.bridge.AS.BotClient()
	url := cli.BuildBaseURL("_matrix", "client", "unstable", "com.beeper.asmux", "pong")
	user.log.Debugfln("Sending bridge state to asmux: %+v", state)
	_, err := cli.MakeRequest("POST", url, &state, nil)
	if err != nil {
		user.log.Warnln("Failed to update bridge state in asmux:", err)
	} else {
		user.prevBridgeStatus = &state
	}
}

var asmuxPingID uint32 = 0

func (prov *ProvisioningAPI) AsmuxPing(w http.ResponseWriter, r *http.Request) {
	if !prov.bridge.AS.CheckServerToken(w, r) {
		return
	}
	userID := r.URL.Query().Get("user_id")
	user := prov.bridge.GetUserByMXID(id.UserID(userID))
	var resp AsmuxPong
	if user.Pulse == nil {
		if user.Session == nil {
			resp.Error = AsmuxWANotLoggedIn
		} else {
			resp.Error = AsmuxWANotConnected
		}
	} else {
		if user.Pulse.IsConnected() {
			pingID := atomic.AddUint32(&asmuxPingID, 1)
			user.log.Debugfln("Pinging WhatsApp mobile due to asmux /ping API request (ID %d)", pingID)
			// err := user.Conn.AdminTestWithSuppress(true)
			// if errors.Is(r.Context().Err(), context.Canceled) {
			// 	user.log.Warnfln("Ping request %d was canceled before we responded (response was %v)", pingID, err)
			// 	user.prevBridgeStatus = nil
			// 	return
			// }
			resp.OK = true
			// user.log.Debugfln("Ping %d response: %v", pingID, err)
			// if err == whatsapp.ErrPingFalse {
			// 	user.log.Debugln("Forwarding ping false error from provisioning API to HandleError")
			// 	go user.HandleError(err)
			// 	resp.Error = AsmuxWAPingFalse
			// } else if errors.Is(err, whatsapp.ErrConnectionTimeout) {
			// 	resp.Error = AsmuxWATimeout
			// } else if err != nil {
			// 	resp.Error = AsmuxWAPingError
			// } else {
			// 	resp.OK = true
			// }
			// } else if user.Conn.IsLoginInProgress() {
			// 	resp.Error = AsmuxWAConnecting
			// } else if user.Conn.IsConnected() {
			// 	resp.Error = AsmuxWANotLoggedIn
		} else {
			resp.Error = AsmuxWANotConnected
		}
	}
	resp.fill()
	user.log.Debugfln("Responding bridge state to asmux: %+v", resp)
	jsonResponse(w, http.StatusOK, &resp)
	user.prevBridgeStatus = &resp
}
