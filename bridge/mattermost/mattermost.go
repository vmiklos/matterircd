package mattermost

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/42wim/matterbridge/matterclient"
	"github.com/42wim/matterircd/bridge"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mitchellh/mapstructure"
	logger "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type Mattermost struct {
	mc          *matterclient.MMClient
	credentials bridge.Credentials
	idleStop    chan struct{}
	eventChan   chan *bridge.Event
	v           *viper.Viper
}

func New(v *viper.Viper, cred bridge.Credentials, eventChan chan *bridge.Event, onWsConnect func()) (bridge.Bridger, *matterclient.MMClient, error) {
	m := &Mattermost{
		credentials: cred,
		eventChan:   eventChan,
		v:           v,
	}

	if v.GetBool("debug") {
		logger.SetLevel(logger.DebugLevel)
	}

	if v.GetBool("trace") {
		logger.SetLevel(logger.TraceLevel)
	}

	fmt.Println("loggerlevel:", logger.GetLevel())

	mc, err := m.loginToMattermost()
	if err != nil {
		return nil, nil, err
	}

	mc.EnableAllEvents()

	m.mc.OnWsConnect = onWsConnect
	go mc.StatusLoop()

	m.mc = mc

	return m, mc, nil
}

func (m *Mattermost) loginToMattermost() (*matterclient.MMClient, error) {
	mc := matterclient.New(m.credentials.Login, m.credentials.Pass, m.credentials.Team, m.credentials.Server)
	if m.v.GetBool("mattermost.Insecure") {
		mc.Credentials.NoTLS = true
	}

	mc.Credentials.SkipTLSVerify = m.v.GetBool("mattermost.SkipTLSVerify")

	/*
		if m.v.GetBool("debug") {
			mc.SetLogLevel("debug")
		}
	*/

	logger.Infof("login as %s (team: %s) on %s", m.credentials.Login, m.credentials.Team, m.credentials.Server)

	err := mc.Login()
	if err != nil {
		logger.Error("login failed", err)
		return nil, err
	}

	logger.Info("login succeeded")

	m.mc = mc

	m.mc.WsQuit = false

	go mc.WsReceiver()
	go m.handleWsMessage()

	// do anti idle on town-square, every installation should have this channel
	channels := m.mc.GetChannels()
	for _, channel := range channels {
		if channel.Name == "town-square" && !m.v.GetBool("mattermost.DisableAutoView") {
			go m.antiIdle(channel.Id)
			continue
		}
	}

	return mc, nil
}

func (m *Mattermost) handleWsMessage() {
	updateChannelsThrottle := time.NewTicker(time.Second * 60)

	for {
		if m.mc.WsQuit {
			logger.Debug("exiting handleWsMessage")
			return
		}

		logger.Debug("in handleWsMessage", len(m.mc.MessageChan))

		message := <-m.mc.MessageChan

		logger.Debugf("MMUser WsReceiver: %#v", message.Raw)
		logger.Tracef("handleWsMessage %s", spew.Sdump(message))
		// check if we have the users/channels in our cache. If not update
		m.checkWsActionMessage(message.Raw, updateChannelsThrottle)

		switch message.Raw.Event {
		case model.WEBSOCKET_EVENT_POSTED:
			m.handleWsActionPost(message.Raw)
		case model.WEBSOCKET_EVENT_POST_EDITED:
			m.handleWsActionPost(message.Raw)
		case model.WEBSOCKET_EVENT_USER_REMOVED:
			m.handleWsActionUserRemoved(message.Raw)
		case model.WEBSOCKET_EVENT_USER_ADDED:
			m.handleWsActionUserAdded(message.Raw)
		case model.WEBSOCKET_EVENT_CHANNEL_CREATED:
			m.handleWsActionChannelCreated(message.Raw)
		case model.WEBSOCKET_EVENT_CHANNEL_DELETED:
			m.handleWsActionChannelDeleted(message.Raw)
		case model.WEBSOCKET_EVENT_USER_UPDATED:
			m.handleWsActionUserUpdated(message.Raw)
		case model.WEBSOCKET_EVENT_STATUS_CHANGE:
			m.handleStatusChangeEvent(message.Raw)
		}
	}
}

func (m *Mattermost) checkWsActionMessage(rmsg *model.WebSocketEvent, throttle *time.Ticker) {
	if m.GetChannelName(rmsg.Broadcast.ChannelId) != "" {
		return
	}

	select {
	case <-throttle.C:
		logger.Debugf("Updating channels for %#v", rmsg.Broadcast)
		go m.UpdateChannels()
	default:
	}
}

// antiIdle does a lastviewed every 60 seconds so that the user is shown as online instead of away
func (m *Mattermost) antiIdle(channelID string) {
	ticker := time.NewTicker(time.Second * 60)

	for {
		select {
		case <-m.idleStop:
			logger.Debug("stopping antiIdle loop")
			return
		case <-ticker.C:
			if m.mc == nil {
				logger.Error("antiidle: don't have a connection, exiting loop.")
				return
			}

			m.mc.UpdateLastViewed(channelID)
		}
	}
}

func (m *Mattermost) Invite(channelID, username string) error {
	_, resp := m.mc.Client.AddChannelMember(channelID, username)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Join(channelName string) (string, string, error) {
	teamID := ""

	sp := strings.Split(channelName, "/")
	if len(sp) > 1 {
		team, _ := m.mc.Client.GetTeamByName(sp[0], "")
		if team == nil {
			return "", "", fmt.Errorf("cannot join channel (+i)")
		}

		teamID = team.Id
		channelName = sp[1]
	}

	if teamID == "" {
		teamID = m.mc.Team.Id
	}

	channelID := m.mc.GetChannelId(channelName, teamID)

	err := m.mc.JoinChannel(channelID)
	logger.Debugf("join channel %s, id %s, err: %v", channelName, channelID, err)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i)")
	}

	topic := m.mc.GetChannelHeader(channelID)

	return channelID, topic, nil
}

func (m *Mattermost) List() (map[string]string, error) {
	channelinfo := make(map[string]string)

	for _, channel := range append(m.mc.GetChannels(), m.mc.GetMoreChannels()...) {
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		if strings.Contains(channel.Name, "__") {
			continue
		}

		channelName := "#" + channel.Name
		// prefix channels outside of our team with team name
		if channel.TeamId != m.mc.Team.Id {
			channelName = m.mc.GetTeamName(channel.TeamId) + "/" + channel.Name
		}

		channelinfo[channelName] = strings.ReplaceAll(channel.Header, "\n", " | ")
	}

	return channelinfo, nil
}

func (m *Mattermost) Part(channelID string) error {
	m.mc.Client.RemoveUserFromChannel(channelID, m.mc.User.Id)

	return nil
}

func (m *Mattermost) UpdateChannels() error {
	return m.mc.UpdateChannels()
}

func (m *Mattermost) Logout() error {
	if m.mc.WsClient != nil {
		err := m.mc.Logout()
		if err != nil {
			logger.Error("logout failed")
		}
		logger.Info("logout succeeded")

		m.idleStop <- struct{}{}
	}

	return nil
}

func (m *Mattermost) MsgUser(username, text string) error {
	props := make(map[string]interface{})

	props["matterircd_"+m.mc.User.Id] = true
	m.mc.SendDirectMessageProps(username, text, "", props)

	return nil
}

func (m *Mattermost) MsgChannel(channelID, text string) error {
	props := make(map[string]interface{})
	props["matterircd_"+m.mc.User.Id] = true

	post := &model.Post{ChannelId: channelID, Message: text, Props: props}
	_, resp := m.mc.Client.CreatePost(post)

	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Topic(channelID string) string {
	return m.mc.GetChannelHeader(channelID)
}

func (m *Mattermost) SetTopic(channelID, text string) error {
	logger.Debugf("updating channelheader %#v, %#v", channelID, text)
	patch := &model.ChannelPatch{
		Header: &text,
	}

	_, resp := m.mc.Client.PatchChannel(channelID, patch)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) StatusUser(userID string) (string, error) {
	return m.mc.GetStatus(userID), nil
}

func (m *Mattermost) StatusUsers() (map[string]string, error) {
	return m.mc.GetStatuses(), nil
}

func (m *Mattermost) Protocol() string {
	return "mattermost"
}

func (m *Mattermost) Kick(channelID, username string) error {
	_, resp := m.mc.Client.RemoveUserFromChannel(channelID, username)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) SetStatus(status string) error {
	_, resp := m.mc.Client.UpdateUserStatus(m.mc.User.Id, &model.Status{
		Status: status,
		UserId: m.mc.User.Id,
	})
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Nick(name string) error {
	return m.mc.UpdateUserNick(name)
}

func (m *Mattermost) GetChannelName(channelID string) string {
	var name string

	channelName := m.mc.GetChannelName(channelID)
	teamID := m.mc.GetTeamFromChannel(channelID)
	teamName := m.mc.GetTeamName(teamID)

	if channelName != "" {
		if (teamName != "" && teamID != m.mc.Team.Id) || m.v.GetBool("mattermost.PrefixMainTeam") {
			name = "#" + teamName + "/" + channelName
		}
		if teamID == m.mc.Team.Id && !m.v.GetBool("mattermost.PrefixMainTeam") {
			name = "#" + channelName
		}
		if teamID == "G" {
			name = "#" + channelName
		}
	} else {
		name = channelID
	}

	return name
}

func (m *Mattermost) GetChannelUsers(channelID string) ([]*bridge.UserInfo, error) {
	var (
		mmusers []*model.User
		users   []*bridge.UserInfo
	)

	idx := 0
	max := 200

	mmusersPaged, resp := m.mc.Client.GetUsersInChannel(channelID, idx, max, "")
	if resp.Error != nil {
		return nil, resp.Error
	}

	for len(mmusersPaged) > 0 {
		mmusersPaged, resp = m.mc.Client.GetUsersInChannel(channelID, idx, max, "")
		if resp.Error != nil {
			return nil, resp.Error
		}

		idx++

		time.Sleep(time.Millisecond * 200)

		mmusers = append(mmusers, mmusersPaged...)
	}

	for _, mmuser := range mmusers {
		users = append(users, m.createUser(mmuser))
	}

	return users, nil
}

func (m *Mattermost) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	for _, mmuser := range m.mc.GetUsers() {
		users = append(users, m.createUser(mmuser))
	}

	return users
}

func (m *Mattermost) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	for _, mmchannel := range m.mc.GetChannels() {
		channels = append(channels, &bridge.ChannelInfo{
			Name:   mmchannel.Name,
			ID:     mmchannel.Id,
			TeamID: mmchannel.TeamId,
		})
	}

	return channels
}

func (m *Mattermost) GetUser(userID string) *bridge.UserInfo {
	return m.createUser(m.mc.GetUser(userID))
}

func (m *Mattermost) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.User)
}

func (m *Mattermost) GetUserByUsername(username string) *bridge.UserInfo {
	mmuser, resp := m.mc.Client.GetUserByUsername(username, "")
	if resp.Error != nil {
		return &bridge.UserInfo{}
	}

	return m.createUser(mmuser)
}

func (m *Mattermost) createUser(mmuser *model.User) *bridge.UserInfo {
	teamID := ""

	if mmuser == nil {
		return &bridge.UserInfo{}
	}

	nick := mmuser.Username
	if m.v.GetBool("mattermost.PreferNickname") && isValidNick(mmuser.Nickname) {
		nick = mmuser.Nickname
	}

	me := false

	if mmuser.Id == m.mc.User.Id {
		me = true
		teamID = m.mc.Team.Id
	}

	info := &bridge.UserInfo{
		Nick:      nick,
		User:      mmuser.Id,
		Real:      mmuser.FirstName + " " + mmuser.LastName,
		Host:      m.mc.Client.Url,
		Roles:     mmuser.Roles,
		Ghost:     true,
		Me:        me,
		TeamID:    teamID,
		Username:  mmuser.Username,
		FirstName: mmuser.FirstName,
		LastName:  mmuser.LastName,
	}

	return info
}

func isValidNick(s string) bool {
	/* IRC RFC ([0] - see below) mentions a limit of 9 chars for
	 * IRC nicks, but modern clients allow more than that. Let's
	 * use a "sane" big value, the triple of the spec.
	 */
	if len(s) < 1 || len(s) > 27 {
		return false
	}

	/* According to IRC RFC [0], the allowed chars to have as nick
	 * are: ( letter / special-'-' ).*( letter / digit / special ),
	 * where:
	 * letter = [a-z / A-Z]; digit = [0-9];
	 * special = [';', '[', '\', ']', '^', '_', '`', '{', '|', '}', '-']
	 *
	 * ASCII codes (decimal) for the allowed chars:
	 * letter = [65-90,97-122]; digit = [48-57]
	 * special = [59, 91-96, 123-125, 45]
	 * [0] RFC 2812 (tools.ietf.org/html/rfc2812)
	 */

	if s[0] != 59 && (s[0] < 65 || s[0] > 125) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if s[i] != 45 && s[i] != 59 && (s[i] < 65 || s[i] > 125) {
			if s[i] < 48 || s[i] > 57 {
				return false
			}
		}
	}

	return true
}

func (m *Mattermost) wsActionPostSkip(rmsg *model.WebSocketEvent) bool {
	data := model.PostFromJson(strings.NewReader(rmsg.Data["post"].(string)))
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Data["post"].(string)))["props"].(map[string]interface{})

	if rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED && data.HasReactions {
		logger.Debugf("edit post with reactions, do not relay. We don't know if a reaction is added or the post has been edited")
		return true
	}

	if data.UserId == m.GetMe().User {
		if _, ok := extraProps["matterircd_"+m.GetMe().User].(bool); ok {
			logger.Debugf("message is sent from matterirc, not relaying %#v", data.Message)
			return true
		}

		if data.Type == model.POST_JOIN_LEAVE || data.Type == model.POST_JOIN_CHANNEL {
			logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
			return true
		}
	}

	return false
}

// nolint:funlen,gocognit,gocyclo
func (m *Mattermost) handleWsActionPost(rmsg *model.WebSocketEvent) {
	data := model.PostFromJson(strings.NewReader(rmsg.Data["post"].(string)))
	props := rmsg.Data
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Data["post"].(string)))["props"].(map[string]interface{})

	logger.Debugf("handleWsActionPost() receiving userid %s", data.UserId)
	if m.wsActionPostSkip(rmsg) {
		return
	}

	// nolint:nestif
	if data.ParentId != "" {
		parentPost, resp := m.mc.Client.GetPost(data.ParentId, "")
		if resp.Error != nil {
			logger.Errorf("Unable to get parent post for %#v", data)
		} else {
			parentGhost := m.GetUser(parentPost.UserId)
			if m.v.GetBool("mattermost.HideReplies") {
				data.Message = fmt.Sprintf("%s (re @%s)", data.Message, parentGhost.Nick)
			} else {
				data.Message = fmt.Sprintf("%s (re @%s: %s)", data.Message, parentGhost.Nick, parentPost.Message)
			}
		}
	}

	// create new "ghost" user
	ghost := m.GetUser(data.UserId)
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == m.GetMe().User {
		ghost = m.GetMe()
	}

	if ghost == nil {
		ghost = &bridge.UserInfo{
			Nick: data.UserId,
		}
	}

	// if we got attachments (eg slack attachments) and we have a fallback message, show this.
	if entries, ok := extraProps["attachments"].([]interface{}); ok {
		for _, entry := range entries {
			if f, ok := entry.(map[string]interface{}); ok {
				data.Message = data.Message + "\n" + f["fallback"].(string)
			}
		}
	}

	// check if we have a override_username (from webhooks) and use it
	overrideUsername, _ := extraProps["override_username"].(string)
	if overrideUsername != "" {
		// only allow valid irc nicks
		re := regexp.MustCompile("^[a-zA-Z0-9_]*$")
		if re.MatchString(overrideUsername) {
			ghost.Nick = overrideUsername
		}
	}

	if data.Type == model.POST_JOIN_LEAVE || data.Type == "system_leave_channel" ||
		data.Type == "system_join_channel" || data.Type == "system_add_to_channel" ||
		data.Type == "system_remove_from_channel" {
		logger.Debugf("join/leave message. not relaying %#v", data.Message)
		m.UpdateChannels()

		m.wsActionPostJoinLeave(data, extraProps)
		return
	}

	if data.Type == "system_header_change" {
		if topic, ok := extraProps["new_header"].(string); ok {
			if topicuser, ok := extraProps["username"].(string); ok {
				event := &bridge.Event{
					Type: "channel_topic",
					Data: &bridge.ChannelTopicEvent{
						Text:      topic,
						ChannelID: data.ChannelId,
						Sender:    topicuser,
					},
				}
				m.eventChan <- event
			}
		}
	}

	msgs := strings.Split(data.Message, "\n")

	channelType := ""
	if t, ok := props["channel_type"].(string); ok {
		channelType = t
	}

	// add an edited string when messages are edited
	if len(msgs) > 0 && rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED {
		msgs[len(msgs)-1] = msgs[len(msgs)-1] + " (edited)"

		// check if we have an edited direct message (channels have __)
		name := m.GetChannelName(data.ChannelId)
		if strings.Contains(name, "__") {
			channelType = "D"
		}
	}

	for _, msg := range msgs {
		if msg == "" {
			continue
		}

		switch {
		// DirectMessage
		case channelType == "D":
			event := &bridge.Event{
				Type: "direct_message",
			}

			d := &bridge.DirectMessageEvent{
				Text:  msg,
				Files: m.getFilesFromData(data),
			}

			d.Sender = ghost
			d.Receiver = m.GetMe()

			event.Data = d

			m.eventChan <- event
		case strings.Contains(data.Message, "@channel") || strings.Contains(data.Message, "@here") ||
			strings.Contains(data.Message, "@all"):
			event := &bridge.Event{
				Type: "channel_message",
				Data: &bridge.ChannelMessageEvent{
					Text:        msg,
					ChannelID:   data.ChannelId,
					Sender:      ghost,
					MessageType: "notice",
					ChannelType: channelType,
					Files:       m.getFilesFromData(data),
				},
			}

			m.eventChan <- event
		default:
			event := &bridge.Event{
				Type: "channel_message",
				Data: &bridge.ChannelMessageEvent{
					Text:        msg,
					ChannelID:   data.ChannelId,
					Sender:      ghost,
					ChannelType: channelType,
					Files:       m.getFilesFromData(data),
				},
			}

			m.eventChan <- event
		}
	}

	m.handleFileEvent(channelType, ghost, data, props)

	logger.Debugf("handleWsActionPost() user %s sent %s", m.mc.GetUser(data.UserId).Username, data.Message)
	logger.Debugf("%#v", data)

	// updatelastviewed
	if !m.v.GetBool("mattermost.DisableAutoView") {
		m.mc.UpdateLastViewed(data.ChannelId)
	}
}

func (m *Mattermost) getFilesFromData(data *model.Post) []*bridge.File {
	files := []*bridge.File{}

	for _, fname := range m.mc.GetFileLinks(data.FileIds) {
		files = append(files, &bridge.File{
			Name: fname,
		})
	}

	return files
}

func (m *Mattermost) handleFileEvent(channelType string, ghost *bridge.UserInfo, data *model.Post, props map[string]interface{}) {
	event := &bridge.Event{
		Type: "file_event",
	}

	fileEvent := &bridge.FileEvent{
		Sender:      ghost,
		Receiver:    ghost,
		ChannelType: channelType,
		ChannelID:   data.ChannelId,
	}

	event.Data = fileEvent

	for _, fname := range m.getFilesFromData(data) {
		fileEvent.Files = append(fileEvent.Files, &bridge.File{
			Name: fname.Name,
		})
	}

	if len(fileEvent.Files) > 0 {
		switch {
		case channelType == "D":
			fileEvent.Sender = ghost
			fileEvent.Receiver = m.GetMe()

			m.eventChan <- event
		default:
			m.eventChan <- event
		}
	}
}

func (m *Mattermost) wsActionPostJoinLeave(data *model.Post, extraProps map[string]interface{}) {
	switch data.Type {
	case "system_add_to_channel":
		if added, ok := extraProps["addedUsername"].(string); ok {
			if adder, ok := extraProps["username"].(string); ok {
				event := &bridge.Event{
					Type: "channel_add",
					Data: &bridge.ChannelAddEvent{
						Added: []*bridge.UserInfo{
							m.GetUserByUsername(added),
						},
						Adder:     m.GetUserByUsername(adder),
						ChannelID: data.ChannelId,
					},
				}

				m.eventChan <- event
			}
		}
	case "system_remove_from_channel":
		if removed, ok := extraProps["removedUsername"].(string); ok {
			event := &bridge.Event{
				Type: "channel_remove",
				Data: &bridge.ChannelRemoveEvent{
					Removed: []*bridge.UserInfo{
						m.GetUserByUsername(removed),
					},
					ChannelID: data.ChannelId,
				},
			}
			m.eventChan <- event
		}
	}
}

func (m *Mattermost) handleWsActionUserAdded(rmsg *model.WebSocketEvent) {
	userID, ok := rmsg.Data["user_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				m.GetUser(userID),
			},
			Adder: &bridge.UserInfo{
				Nick: "system",
			},
			ChannelID: rmsg.Broadcast.ChannelId,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserRemoved(rmsg *model.WebSocketEvent) {
	userID, ok := rmsg.Data["user_id"].(string)
	if !ok {
		userID = rmsg.Broadcast.UserId
	}

	removerID, ok := rmsg.Data["remover_id"].(string)
	if !ok {
		fmt.Println("not ok removerID", removerID)
		return
	}

	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		channelID = rmsg.Broadcast.ChannelId
	}

	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover: m.GetUser(removerID),
			Removed: []*bridge.UserInfo{
				m.GetUser(userID),
			},
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserUpdated(rmsg *model.WebSocketEvent) {
	var info model.User

	err := Decode(rmsg.Data["user"], &info)
	if err != nil {
		fmt.Println("decode", err)
		return
	}

	event := &bridge.Event{
		Type: "user_updated",
		Data: &bridge.UserUpdateEvent{
			User: m.createUser(&info),
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelCreated(rmsg *model.WebSocketEvent) {
	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_create",
		Data: &bridge.ChannelCreateEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelDeleted(rmsg *model.WebSocketEvent) {
	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_delete",
		Data: &bridge.ChannelDeleteEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleStatusChangeEvent(rmsg *model.WebSocketEvent) {
	var info model.Status

	err := Decode(rmsg.Data, &info)
	if err != nil {
		fmt.Println("decode", err)

		return
	}

	event := &bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: info.UserId,
			Status: info.Status,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) GetTeamName(teamID string) string {
	return m.mc.GetTeamName(teamID)
}

func (m *Mattermost) GetLastViewedAt(channelID string) int64 {
	return m.mc.GetLastViewedAt(channelID)
}

func (m *Mattermost) GetPostsSince(channelID string, since int64) interface{} {
	return m.mc.GetPostsSince(channelID, since)
}

func (m *Mattermost) UpdateLastViewed(channelID string) {
	m.mc.UpdateLastViewed(channelID)
}

func (m *Mattermost) UpdateLastViewedUser(userID string) error {
	dc, resp := m.mc.Client.CreateDirectChannel(m.mc.User.Id, userID)
	if resp.Error != nil {
		return resp.Error
	}

	return m.mc.UpdateLastViewed(dc.Id)
}

func (m *Mattermost) SearchPosts(search string) interface{} {
	return m.mc.SearchPosts(search)
}

func (m *Mattermost) GetFileLinks(fileIDs []string) []string {
	return m.mc.GetFileLinks(fileIDs)
}

func (m *Mattermost) SearchUsers(query string) ([]*bridge.UserInfo, error) {
	users, resp := m.mc.Client.SearchUsers(&model.UserSearch{Term: query})
	if resp.Error != nil {
		return nil, resp.Error
	}

	var brusers []*bridge.UserInfo

	for _, u := range users {
		brusers = append(brusers, m.createUser(u))
	}

	return brusers, nil
}

func (m *Mattermost) GetPosts(channelID string, limit int) interface{} {
	return m.mc.GetPosts(channelID, limit)
}

func (m *Mattermost) GetChannelID(name, teamID string) string {
	return m.mc.GetChannelId(name, teamID)
}

func Decode(input interface{}, output interface{}) error {
	config := &mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   output,
		TagName:  "json",
	}

	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}

	return decoder.Decode(input)
}
