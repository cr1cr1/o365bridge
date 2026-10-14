package graph

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- Tasks (Microsoft To Do) ---

const taskUIDExtensionName = "com.o365bridge.task"

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
	Status            string               `json:"status,omitempty"`
	Importance        string               `json:"importance,omitempty"`
	IsReminderOn      bool                 `json:"isReminderOn,omitempty"`
	ReminderDateTime  *dateTimeTZ          `json:"reminderDateTime,omitempty"`
	DueDateTime       *dateTimeTZ          `json:"dueDateTime,omitempty"`
	StartDateTime     *dateTimeTZ          `json:"startDateTime,omitempty"`
	CompletedDateTime *dateTimeTZ          `json:"completedDateTime,omitempty"`
	Categories        []string             `json:"categories"`
	HasAttachments    bool                 `json:"hasAttachments"`
	Recurrence        *graphTaskRecurrence `json:"recurrence"`
	Extensions        []graphTaskExtension `json:"extensions,omitempty"`
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
	t := bridge.Task{ID: g.ID, UID: g.ID, ListID: listID, Title: g.Title, Status: taskStatusFromGraph(g.Status), Priority: priorityFromGraph(g.Importance)}
	var startDateOnly, dueDateOnly *bool
	for _, extension := range g.Extensions {
		if extension.ExtensionName == taskUIDExtensionName {
			if extension.UID != "" {
				t.UID = extension.UID
			}
			startDateOnly, dueDateOnly = extension.StartDateOnly, extension.DueDateOnly
			break
		}
	}
	t.Categories = g.Categories
	if g.Body != nil {
		t.Body = bridge.PlainTextBody(g.Body.Content, g.Body.ContentType)
	}
	if g.Recurrence != nil {
		t.Recur, t.RecurStart, t.RecurTimeZone, t.RecurUnsupported = g.Recurrence.taskRepeat()
	}
	if g.DueDateTime != nil {
		v := fromGraphDT(*g.DueDateTime)
		t.Due = &v
		t.DueDateOnly = graphTaskDateOnly(g.DueDateTime.DateTime, dueDateOnly)
	}
	if g.StartDateTime != nil {
		v := fromGraphDT(*g.StartDateTime)
		t.Start = &v
		t.StartDateOnly = graphTaskDateOnly(g.StartDateTime.DateTime, startDateOnly)
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

func taskJSON(t bridge.Task, update bool) (map[string]any, error) {
	m := map[string]any{}
	authoritative := !update || t.FullUpdate
	if authoritative || t.Title != "" {
		m["title"] = t.Title
	}
	if authoritative || t.Status != "" {
		m["status"] = taskStatusToGraph(t.Status)
	}
	if authoritative || t.Priority != 0 {
		m["importance"] = importanceToGraph(t.Priority)
	}
	clear := update && t.FullUpdate
	if (!update || !t.BodyUnchanged) && (t.Body != "" || clear) {
		m["body"] = map[string]any{"content": bridge.TaskNoteHTML(t.Body), "contentType": "html"}
	}
	if t.Categories != nil {
		m["categories"] = t.Categories
	}
	if clear {
		for _, field := range []string{"dueDateTime", "startDateTime", "completedDateTime", "reminderDateTime"} {
			m[field] = nil
		}
		m["isReminderOn"] = false
	}
	if t.Due != nil {
		m["dueDateTime"] = toGraphDT(*t.Due, "UTC")
	}
	if t.Start != nil {
		m["startDateTime"] = toGraphDT(*t.Start, "UTC")
	}
	if t.Completed != nil {
		m["completedDateTime"] = toGraphDT(*t.Completed, "UTC")
		m["status"] = "completed"
	}
	if t.Reminder != nil {
		m["reminderDateTime"] = toGraphDT(*t.Reminder, "UTC")
		m["isReminderOn"] = true
	}
	if t.RecurSet || (!update && t.Recur != "") {
		if t.Recur == "" {
			if update {
				m["recurrence"] = nil
			}
		} else {
			recurrence, err := taskRecurrenceJSON(t)
			if err != nil {
				return nil, err
			}
			m["recurrence"] = recurrence
		}
	}
	if update && t.StatusUnchanged {
		delete(m, "status")
		delete(m, "completedDateTime")
	}
	return m, nil
}

func (p *provider) ListTasks(ctx context.Context, listID string) ([]bridge.Task, error) {
	query := url.Values{"$expand": {"extensions($filter=id eq '" + taskUIDExtensionName + "')"}}
	items, _, err := getAll[graphTask](ctx, p, "/me/todo/lists/"+esc(listID)+"/tasks?"+query.Encode(), 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Task, 0, len(items))
	for _, it := range items {
		task := it.toTask(listID)
		if it.HasAttachments {
			attachments, err := p.listTaskAttachments(ctx, listID, it.ID)
			if err != nil {
				return nil, err
			}
			for _, attachment := range attachments {
				if !attachment.isFile() {
					continue
				}
				if attachment.ID == "" || attachment.Size < 0 {
					return nil, bridge.Wrap(bridge.KindOther, "graph: invalid task attachment metadata", nil)
				}
				task.Attachments = append(task.Attachments, bridge.TaskAttachment{ID: attachment.ID, Name: attachment.Name, ContentType: attachment.ContentType, Size: attachment.Size})
			}
		}
		out = append(out, task)
	}
	return out, nil
}

func (p *provider) CreateTask(ctx context.Context, t bridge.Task) (bridge.Task, error) {
	body, err := taskJSON(t, false)
	if err != nil {
		return bridge.Task{}, err
	}
	if t.UID != "" || t.StartDateOnly || t.DueDateOnly {
		// Identity and date fidelity are persisted atomically with task creation.
		body["extensions"] = []any{taskMetadataJSON(t.UID, t.Start != nil && t.StartDateOnly, t.Due != nil && t.DueDateOnly)}
	}
	var gt graphTask
	if err := p.doJSON(ctx, http.MethodPost, "/me/todo/lists/"+esc(t.ListID)+"/tasks", body, &gt); err != nil {
		return bridge.Task{}, err
	}
	if gt.ID == "" {
		return bridge.Task{}, bridge.Wrap(bridge.KindOther, "graph: create task response missing id", nil)
	}
	out := gt.toTask(t.ListID)
	out.StartDateOnly, out.DueDateOnly = t.Start != nil && t.StartDateOnly, t.Due != nil && t.DueDateOnly
	if t.UID != "" {
		out.UID = t.UID // A create response need not expand extensions.
	}
	return out, nil
}

func (p *provider) UpdateTask(ctx context.Context, t bridge.Task) error {
	body, err := taskJSON(t, true)
	if err != nil {
		return err
	}
	metadataMethod, metadata, err := p.taskDateMetadataChange(ctx, t)
	if err != nil {
		return err
	}
	path := "/me/todo/lists/" + esc(t.ListID) + "/tasks/" + esc(t.ID)
	if err := p.doJSON(ctx, http.MethodPatch, path, body, nil); err != nil {
		return err
	}
	if metadataMethod != "" {
		// Graph task and extension writes are not atomic; never hide metadata failure.
		path += "/extensions"
		if metadataMethod == http.MethodPatch {
			path += "/" + esc(taskUIDExtensionName)
		}
		return p.doJSON(ctx, metadataMethod, path, metadata, nil)
	}
	return nil
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
