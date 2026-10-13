package graph

import (
	"context"
	"net/http"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- Tasks (Microsoft To Do) ---

type graphTaskList struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func (p *provider) ListTaskLists(ctx context.Context) ([]bridge.Calendar, error) {
	items, _, err := getAll[graphTaskList](ctx, p, "/me/todo/lists", 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Calendar, 0, len(items))
	for _, it := range items {
		out = append(out, bridge.Calendar{ID: it.ID, Name: it.DisplayName, TasksOnly: true})
	}
	return out, nil
}

type graphTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  *struct {
		Content     string `json:"content"`
		ContentType string `json:"contentType"`
	} `json:"body,omitempty"`
	Status            string      `json:"status,omitempty"`
	Importance        string      `json:"importance,omitempty"`
	IsReminderOn      bool        `json:"isReminderOn,omitempty"`
	ReminderDateTime  *dateTimeTZ `json:"reminderDateTime,omitempty"`
	DueDateTime       *dateTimeTZ `json:"dueDateTime,omitempty"`
	StartDateTime     *dateTimeTZ `json:"startDateTime,omitempty"`
	CompletedDateTime *dateTimeTZ `json:"completedDateTime,omitempty"`
}

func taskStatusToGraph(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "in-process":
		return "inProgress"
	default:
		return "notStarted"
	}
}
func taskStatusFromGraph(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "inProgress":
		return "in-process"
	default:
		return "needs-action"
	}
}

func importanceToGraph(priority int) string {
	switch {
	case priority >= 1 && priority <= 4:
		return "high"
	case priority >= 6:
		return "low"
	default:
		return "normal"
	}
}
func priorityFromGraph(imp string) int {
	switch imp {
	case "high":
		return 1
	case "low":
		return 9
	default:
		return 5
	}
}

func (g graphTask) toTask(listID string) bridge.Task {
	t := bridge.Task{ID: g.ID, ListID: listID, Title: g.Title, Status: taskStatusFromGraph(g.Status), Priority: priorityFromGraph(g.Importance)}
	if g.Body != nil {
		t.Body = g.Body.Content
	}
	if g.DueDateTime != nil {
		v := fromGraphDT(*g.DueDateTime)
		t.Due = &v
	}
	if g.StartDateTime != nil {
		v := fromGraphDT(*g.StartDateTime)
		t.Start = &v
	}
	if g.CompletedDateTime != nil {
		v := fromGraphDT(*g.CompletedDateTime)
		t.Completed = &v
		t.Status = "completed"
	}
	if g.IsReminderOn && g.ReminderDateTime != nil {
		v := fromGraphDT(*g.ReminderDateTime)
		t.Reminder = &v
	}
	return t
}

func taskJSON(t bridge.Task) map[string]any {
	m := map[string]any{
		"title":      t.Title,
		"status":     taskStatusToGraph(t.Status),
		"importance": importanceToGraph(t.Priority),
	}
	if t.Body != "" {
		m["body"] = map[string]any{"content": t.Body, "contentType": "text"}
	}
	if t.Due != nil {
		d := toGraphDT(*t.Due, "UTC")
		m["dueDateTime"] = d
	}
	if t.Start != nil {
		d := toGraphDT(*t.Start, "UTC")
		m["startDateTime"] = d
	}
	if t.Completed != nil {
		d := toGraphDT(*t.Completed, "UTC")
		m["completedDateTime"] = d
		m["status"] = "completed"
	}
	if t.Reminder != nil {
		d := toGraphDT(*t.Reminder, "UTC")
		m["reminderDateTime"] = d
		m["isReminderOn"] = true
	}
	// Recur (RRULE string) vs Graph structured recurrence: omitted rather
	// than risk corrupting the task; see calendar.go note.
	return m
}

func (p *provider) ListTasks(ctx context.Context, listID string) ([]bridge.Task, error) {
	items, _, err := getAll[graphTask](ctx, p, "/me/todo/lists/"+esc(listID)+"/tasks", 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Task, 0, len(items))
	for _, it := range items {
		out = append(out, it.toTask(listID))
	}
	return out, nil
}

func (p *provider) CreateTask(ctx context.Context, t bridge.Task) (bridge.Task, error) {
	var gt graphTask
	if err := p.doJSON(ctx, http.MethodPost, "/me/todo/lists/"+esc(t.ListID)+"/tasks", taskJSON(t), &gt); err != nil {
		return bridge.Task{}, err
	}
	out := gt.toTask(t.ListID)
	if out.ID == "" {
		out = t
	}
	return out, nil
}

func (p *provider) UpdateTask(ctx context.Context, t bridge.Task) error {
	return p.doJSON(ctx, http.MethodPatch, "/me/todo/lists/"+esc(t.ListID)+"/tasks/"+esc(t.ID), taskJSON(t), nil)
}

func (p *provider) DeleteTask(ctx context.Context, listID, id string) error {
	_, err := p.do(ctx, http.MethodDelete, "/me/todo/lists/"+esc(listID)+"/tasks/"+esc(id), nil, nil)
	return err
}

// --- Contacts ---

type graphContact struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	GivenName      string `json:"givenName"`
	Surname        string `json:"surname"`
	EmailAddresses []struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	} `json:"emailAddresses"`
	BusinessPhones []string `json:"businessPhones"`
	HomePhones     []string `json:"homePhones"`
	MobilePhone    string   `json:"mobilePhone"`
	CompanyName    string   `json:"companyName"`
	PersonalNotes  string   `json:"personalNotes"`
}

func (g graphContact) toContact(bookID string) bridge.Contact {
	c := bridge.Contact{ID: g.ID, BookID: bookID, FullName: g.DisplayName, FirstName: g.GivenName, LastName: g.Surname, Org: g.CompanyName, Notes: g.PersonalNotes}
	if c.FullName == "" {
		c.FullName = strings.TrimSpace(g.GivenName + " " + g.Surname)
	}
	for _, e := range g.EmailAddresses {
		if e.Address != "" {
			c.Emails = append(c.Emails, e.Address)
		}
	}
	c.Phones = append(c.Phones, g.BusinessPhones...)
	c.Phones = append(c.Phones, g.HomePhones...)
	if g.MobilePhone != "" {
		c.Phones = append(c.Phones, g.MobilePhone)
	}
	return c
}

func contactJSON(c bridge.Contact) map[string]any {
	display := c.FullName
	if display == "" {
		display = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	m := map[string]any{
		"displayName":   display,
		"givenName":     c.FirstName,
		"surname":       c.LastName,
		"companyName":   c.Org,
		"personalNotes": c.Notes,
	}
	if len(c.Emails) > 0 {
		var eas []map[string]any
		for _, e := range c.Emails {
			eas = append(eas, map[string]any{"address": e})
		}
		m["emailAddresses"] = eas
	}
	if len(c.Phones) > 0 {
		m["businessPhones"] = c.Phones
	}
	return m
}

func contactsBase(bookID string) string {
	if bookID != "" {
		return "/me/contactFolders/" + esc(bookID) + "/contacts"
	}
	return "/me/contacts"
}

func (p *provider) ListContacts(ctx context.Context, bookID string) ([]bridge.Contact, error) {
	items, _, err := getAll[graphContact](ctx, p, contactsBase(bookID), 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Contact, 0, len(items))
	for _, it := range items {
		out = append(out, it.toContact(bookID))
	}
	return out, nil
}

func (p *provider) CreateContact(ctx context.Context, c bridge.Contact) (bridge.Contact, error) {
	var gc graphContact
	if err := p.doJSON(ctx, http.MethodPost, contactsBase(c.BookID), contactJSON(c), &gc); err != nil {
		return bridge.Contact{}, err
	}
	out := gc.toContact(c.BookID)
	if out.ID == "" {
		out = c
	}
	return out, nil
}

func (p *provider) UpdateContact(ctx context.Context, c bridge.Contact) error {
	return p.doJSON(ctx, http.MethodPatch, "/me/contacts/"+esc(c.ID), contactJSON(c), nil)
}

func (p *provider) DeleteContact(ctx context.Context, bookID, id string) error {
	_, err := p.do(ctx, http.MethodDelete, "/me/contacts/"+esc(id), nil, nil)
	return err
}
