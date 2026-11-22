module tablenet-adapter-hexproof

go 1.24

require (
	github.com/coder/websocket v1.8.15
	tablenet v0.0.0-00010101000000-000000000000
)

require github.com/BurntSushi/toml v1.6.0 // indirect

replace tablenet => ../..
