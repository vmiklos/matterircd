package bridge

import (
	"time"
)

type Bridger interface {
	Invite(channelID, username string) error
	Join(channelName string) (string, string, error)
	List() (map[string]string, error)
	Part(channel string) error
	SetTopic(channelID, text string) error
	Topic(channelID string) string
	Kick(channelID, username string) error
	Nick(name string) error

	UpdateChannels() error
	Logout() error

	MsgUser(username, text string) error
	MsgChannel(channelID, text string) error

	StatusUser(name string) (string, error)
	StatusUsers() (map[string]string, error)
	SetStatus(status string) error

	Protocol() string

	GetChannels() []*ChannelInfo
	GetChannelName(channelID string) string
	GetLastViewedAt(channelID string) int64
	UpdateLastViewed(channelID string)
	UpdateLastViewedUser(userID string) error
	GetChannelID(name, teamID string) string

	GetChannelUsers(channelID string) ([]*UserInfo, error)
	GetUsers() []*UserInfo
	GetUser(userID string) *UserInfo
	GetMe() *UserInfo
	GetUserByUsername(username string) *UserInfo
	SearchUsers(query string) ([]*UserInfo, error)

	GetTeamName(teamID string) string

	GetPostsSince(channelID string, since int64) interface{}
	GetPosts(channelID string, limit int) interface{}
	SearchPosts(search string) interface{}
	GetFileLinks(fileIDs []string) []string
}

type ChannelInfo struct {
	Name   string
	ID     string
	TeamID string
}

type UserInfo struct {
	Nick        string   // From NICK command
	User        string   // From USER command
	Real        string   // From USER command
	Pass        []string // From PASS command
	Host        string
	Roles       string
	DisplayName string
	Ghost       bool
	Me          bool
	Username    string
	TeamID      string
	FirstName   string
	LastName    string
}

type Credentials struct {
	Login  string
	Team   string
	Pass   string
	Server string
	Token  string
}

type Event struct {
	Type string
	Data interface{}
}

type ChannelAddEvent struct {
	Adder     *UserInfo
	Added     []*UserInfo
	ChannelID string
}

type ChannelRemoveEvent struct {
	Remover   *UserInfo
	Removed   []*UserInfo
	ChannelID string
}

type ChannelCreateEvent struct {
	ChannelID string
}

type ChannelDeleteEvent struct {
	ChannelID string
}

type ChannelMessageEvent struct {
	Text        string
	ChannelID   string
	Sender      *UserInfo
	MessageType string
	ChannelType string
	Files       []*File
}

type ChannelTopicEvent struct {
	Text      string
	ChannelID string
	Sender    string
}

type DirectMessageEvent struct {
	Text     string
	Receiver string
	Sender   *UserInfo
	Files    []*File
}

type FileEvent struct {
	Receiver    string
	Sender      *UserInfo
	ChannelID   string
	ChannelType string
	Files       []*File
}

type UserUpdateEvent struct {
	User *UserInfo
}

type File struct {
	Name string
}

type Message struct {
	Text      string    `json:"text"`
	Channel   string    `json:"channel"`
	Username  string    `json:"username"`
	UserID    string    `json:"userid"` // userid on the bridge
	Account   string    `json:"account"`
	Event     string    `json:"event"`
	Protocol  string    `json:"protocol"`
	ParentID  string    `json:"parent_id"`
	Timestamp time.Time `json:"timestamp"`
	ID        string    `json:"id"`
	Extra     map[string][]interface{}
}
