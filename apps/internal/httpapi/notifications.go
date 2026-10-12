package httpapi

import (
	"net/http"
	"strconv"

	"dedie/engine/app"
	"dedie/engine/store"
)

// notificationList is what the notification centre polls: the caller's
// unread count, and the page of notifications asked for.
type notificationList struct {
	Unread int                  `json:"unread"`
	Items  []store.Notification `json:"items"`
}

// listNotifications serves GET /notifications. Query parameters: unread=1
// keeps what the caller has not read, topic keeps one topic, before pages
// back from an id, and limit=0 asks for the count alone.
func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	q := r.URL.Query()
	unread, err := s.app.UnreadNotifications(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	out := notificationList{Unread: unread, Items: []store.Notification{}}
	if q.Get("limit") != "0" {
		query := app.NotificationQuery{Unread: q.Get("unread") == "1", Topic: q.Get("topic")}
		query.Limit, _ = strconv.Atoi(q.Get("limit"))
		query.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
		if out.Items, err = s.app.ListNotifications(r.Context(), actor, query); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) readNotification(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, app.ErrNotFound)
		return
	}
	if err := s.app.MarkNotificationRead(r.Context(), actor, id); err != nil {
		fail(w, err)
		return
	}
	s.unreadCount(w, r, actor)
}

func (s *Server) readAllNotifications(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if _, err := s.app.MarkAllNotificationsRead(r.Context(), actor); err != nil {
		fail(w, err)
		return
	}
	s.unreadCount(w, r, actor)
}

func (s *Server) testNotification(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	s.app.TestNotification(r.Context(), actor)
	s.unreadCount(w, r, actor)
}

// unreadCount answers a change of read state with the count that results,
// so the client's badge is right without asking again.
func (s *Server) unreadCount(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	unread, err := s.app.UnreadNotifications(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unread": unread})
}
