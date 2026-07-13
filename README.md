TGIRC - Telegram to IRC gateway

This app exposes a logged-in telegram account as an IRC server interface.
Main use case is to be able to use Telegram via TUI clients like WeeChat
without having to write full sized client side plugins.


Based on TDLib

# Security

Currently there is no authentication, so anyone who can connect to the service
gets access to Telegram account logged there in.
