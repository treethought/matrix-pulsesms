# matrix-pulsesms

A mautrix-style matrix bridge for [PulseSMS](https://home.pulsesms.app/overview/)

This bridge is a fork of the excellent
[mautrix-whatsapp](https://github.com/tulir/mautrix-whatsapp) created by Tulir
Asokan. All credit for design and heavy lifting of this bridge belongs to him.

## Overview

While there are other options for bridging SMS
[SmsMatrix](https://github.com/tijder/SmsMatrix) and [matrix-sms-bridge](https://github.com/benkuly/matrix-sms-bridge), they require either an SMS gateway server, or a dedicated app running alongside your sms app to work. 

mautrix-pulsesms instead uses the cross-device syncing capability and API of
PulseSMS and serves as a bridge by acting as pulse API client.

While this has some benefits over the other bridges (no gateway needed, and no
dediciated app running in the background), there are some drawbacks and things
to be aware of:
- you will need a cloud account / subscription with PulseSMS
- your messages will be stored in the cloud by PulseSMS, however everything is
  end-to-end encrypted
- The PulseSMS API is not officially supported or documented, so there may be
  some issues.
- I have found that occasionally, messages will be successfully sent to PulseSMS
  and appear on other devices, but are not sent as SMS on the primary (phone)
  device. This can usually be fixed by force quitting the app or restarting your
  device
  
## Usage

After registering the appservice, send a message to @pulsebot:domain. You can
the send `help` to see a list of commands.

To begin bridging
- login by sending `login email@example.com pulsesmspassword 15555555555`, replacing your email, password, and phone number (prefixed with `1`)
- if you enabled the usage of a Space, the bot will invite you to the newly
  created PulseSMS space
- you will then see invites from your most recent conversations.
- Each contact is puppetted in matrix with an mxid of `@pulse_PHONENUMBER:domain` 
- pulsebot will invite your matrix user and the puppets that are members for
  each chat
- SMS conversations with a single contact will be created as direct messages in
  matrix
- Group SMS conversations will be created as rooms in matrix




## Features

After logging in and performing

- [x] syncing of contacts
- [ ] backfilling of history on sync
- [x] SMS for direct chats
- [x] SMS for group chats
- [x] handling of messages from new contacts
- [ ] MMS / media messages
- [ ] Syncing of Read/Seen status
  
