TGIRC - Telegram to IRC gateway
===

This app exposes a logged-in telegram account as an IRC server interface.
Main use case is to be able to use Telegram via TUI clients like WeeChat
without having to write full sized client side plugins.


Depends on go-tdlib and TDLib [link](https://github.com/zelenin/go-tdlib#go-tdlib)

## Security

Currently there is no authentication, so anyone who can connect to the bridge
gets access to Telegram account logged there in.
Recommended usage: listen locally.
