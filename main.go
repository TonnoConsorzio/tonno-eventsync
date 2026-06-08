package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"
)

// Configurazione base
var config = struct {
	MacroProjectName    string
	MacroProjectID      int64
	NewColumnName       string
	ConcludedColumnName string
	TemplateProjectName string
	TemplateProjectID   int64
}{
	MacroProjectName:    "🎠 | Eventi",
	NewColumnName:       "📋 Pianificazione",
	ConcludedColumnName: "📉 Finito",
	TemplateProjectName: "🎪 | Nuovo Evento",
}

// ---------------------------------------------------------
// STRUTTURE DATABASE (Tag puliti per compatibilità Yaegi)
// ---------------------------------------------------------

type EventSyncMapping struct {
	TaskID    int64     `xorm:"pk"`
	ProjectID int64     `xorm:"unique"`
	Created   time.Time `xorm:"created"`
}

func (EventSyncMapping) TableName() string { return "tonno_eventsync_mappings" }

type Project struct {
	ID              int64     `xorm:"pk autoincr"`
	Title           string    `xorm:"not null"`
	Description     string    `xorm:"text"`
	OwnerID         int64     `xorm:"index"`
	ParentProjectID int64     `xorm:"index"`
	Identifier      string    `xorm:"varchar(250)"`
	IsArchived      bool      `xorm:"not null default false"`
	Created         time.Time `xorm:"created"`
	Updated         time.Time `xorm:"updated"`
}

func (Project) TableName() string { return "projects" }

type ProjectView struct {
	ID        int64     `xorm:"pk autoincr"`
	ProjectID int64     `xorm:"not null index"`
	Title     string    `xorm:"text"`
	ViewKind  int64     `xorm:"int"`
	Position  float64   `xorm:"double"`
	Created   time.Time `xorm:"created"`
	Updated   time.Time `xorm:"updated"`
}

func (ProjectView) TableName() string { return "project_views" }

type Bucket struct {
	ID            int64     `xorm:"pk autoincr"`
	Title         string    `xorm:"text not null"`
	ProjectViewID int64     `xorm:"not null index"`
	Position      float64   `xorm:"double"`
	CreatedByID   int64     `xorm:"created_by_id not null"` // <--- AGGIUNGI QUESTA
	Created       time.Time `xorm:"created"`
	Updated       time.Time `xorm:"updated"`
}

func (Bucket) TableName() string { return "buckets" }

type Task struct {
	ID          int64     `xorm:"pk autoincr"`
	Title       string    `xorm:"text not null"`
	Description string    `xorm:"text"`
	Done        bool      `xorm:"index"`
	DoneAt      time.Time `xorm:"index"`
	Priority    int64     `xorm:"index"`
	ProjectID   int64     `xorm:"not null index"`
	BucketID    int64     `xorm:"index"`
	Created     time.Time `xorm:"created"`
	Updated     time.Time `xorm:"updated"`
}

func (Task) TableName() string { return "tasks" }

type TaskRelation struct {
	ID           int64  `xorm:"pk autoincr"`
	TaskID       int64  `xorm:"not null index"`
	OtherTaskID  int64  `xorm:"not null index"`
	RelationKind string `xorm:"varchar(255) not null"`
}

func (TaskRelation) TableName() string { return "task_relations" }

type TaskAssignee struct {
	TaskID  int64     `xorm:"not null pk"`
	UserID  int64     `xorm:"not null pk"`
	Created time.Time `xorm:"created"`
}

func (TaskAssignee) TableName() string { return "task_assignees" }

// ---------------------------------------------------------
// INIZIALIZZAZIONE PLUG-IN
// ---------------------------------------------------------

type TonnoEventSyncPlugin struct{}

var _ plugins.Plugin = (*TonnoEventSyncPlugin)(nil)
var singleton = &TonnoEventSyncPlugin{}

func NewPlugin() plugins.Plugin { return singleton }

func (p *TonnoEventSyncPlugin) Name() string    { return "tonno-eventsync" }
func (p *TonnoEventSyncPlugin) Version() string { return "1.0.0" }

func (p *TonnoEventSyncPlugin) Init() error {
	log.Infof("Initializing %s version %s", p.Name(), p.Version())

	s := db.NewSession()
	defer s.Close()

	if err := s.Begin(); err != nil {
		log.Errorf("[tonno-eventsync] Failed to begin db transaction: %v", err)
		return err
	}

	_, err := s.Exec(`
	CREATE TABLE IF NOT EXISTS tonno_eventsync_mappings (
		task_id BIGINT PRIMARY KEY NOT NULL,
		project_id BIGINT NOT NULL UNIQUE,
		created TIMESTAMP NOT NULL
	);
	`)
	if err != nil {
		log.Errorf("[tonno-eventsync] Failed to create db table: %v", err)
		s.Rollback()
		return err
	}

	if err := s.Commit(); err != nil {
		log.Errorf("[tonno-eventsync] Failed to commit: %v", err)
		return err
	}

	if val := os.Getenv("VIKUNJA_EVENT_MACRO_PROJECT_NAME"); val != "" {
		config.MacroProjectName = val
	}
	if val := os.Getenv("VIKUNJA_EVENT_MACRO_PROJECT_ID"); val != "" {
		if id, err := strconv.ParseInt(val, 10, 64); err == nil {
			config.MacroProjectID = id
		}
	}
	if val := os.Getenv("VIKUNJA_EVENT_NEW_COLUMN_NAME"); val != "" {
		config.NewColumnName = val
	}
	if val := os.Getenv("VIKUNJA_EVENT_CONCLUDED_COLUMN_NAME"); val != "" {
		config.ConcludedColumnName = val
	}
	if val := os.Getenv("VIKUNJA_EVENT_TEMPLATE_PROJECT_NAME"); val != "" {
		config.TemplateProjectName = val
	}
	if val := os.Getenv("VIKUNJA_EVENT_TEMPLATE_PROJECT_ID"); val != "" {
		if id, err := strconv.ParseInt(val, 10, 64); err == nil {
			config.TemplateProjectID = id
		}
	}

	events.RegisterListener("task.created", &TaskCreatedListener{})
	events.RegisterListener("task.updated", &TaskUpdatedListener{})
	events.RegisterListener("project.updated", &ProjectUpdatedListener{})
	events.RegisterListener("task.assignee.created", &TaskAssigneeCreatedListener{})

	return nil
}

func (p *TonnoEventSyncPlugin) Shutdown() error {
	log.Infof("Shutting down %s", p.Name())
	return nil
}

// ---------------------------------------------------------
// LOGICA EVENTI
// ---------------------------------------------------------

type TaskCreatedListener struct{}

func (l *TaskCreatedListener) Handle(msg *message.Message) error {
	var event models.TaskCreatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		log.Errorf("[tonno-eventsync] Failed to unmarshal TaskCreatedEvent: %v", err)
		return err
	}

	if event.Task == nil {
		return nil
	}

	s := db.NewSession()
	defer s.Close()
	if err := s.Begin(); err != nil {
		return err
	}

	var macroProjID int64
	if config.MacroProjectID > 0 {
		macroProjID = config.MacroProjectID
	} else {
		var p Project
		has, err := s.Table("projects").Where("title = ?", config.MacroProjectName).Get(&p)
		if err != nil {
			s.Rollback()
			return err
		}
		if has {
			macroProjID = p.ID
		}
	}

	if macroProjID == 0 || event.Task.ProjectID != macroProjID {
		s.Rollback()
		return nil
	}

	if event.Task.BucketID > 0 {
		type BucketCheck struct {
			ID    int64  `xorm:"id"`
			Title string `xorm:"title"`
		}
		var b BucketCheck
		has, err := s.Table("buckets").Where("id = ?", event.Task.BucketID).Get(&b)
		if err != nil {
			s.Rollback()
			return err
		}
		if !has || !strings.EqualFold(b.Title, config.NewColumnName) {
			s.Rollback()
			return nil
		}
	} else {
		s.Rollback()
		return nil
	}

	var existing EventSyncMapping
	hasMap, err := s.Table("tonno_eventsync_mappings").Where("task_id = ?", event.Task.ID).Get(&existing)
	if err != nil || hasMap {
		s.Rollback()
		return err
	}

	evtIdentifier, err := getUniqueProjectIdentifier()
	if err != nil {
		s.Rollback()
		return err
	}

	var ownerID int64
	if event.Doer != nil && event.Doer.ID > 0 {
		ownerID = event.Doer.ID
	} else {
		type DBUser struct {
			ID int64 `xorm:"id"`
		}
		var u DBUser
		hasUser, err := s.Table("users").Limit(1).Get(&u)
		if err == nil && hasUser {
			ownerID = u.ID
		} else {
			ownerID = 1
		}
	}

	newProj := Project{
		Title:           event.Task.Title,
		ParentProjectID: macroProjID,
		OwnerID:         ownerID,
		Identifier:      evtIdentifier,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	if _, err := s.Table("projects").Insert(&newProj); err != nil {
		s.Rollback()
		return fmt.Errorf("failed to insert new sub-project: %w", err)
	}

	mapping := EventSyncMapping{
		TaskID:    event.Task.ID,
		ProjectID: newProj.ID,
		Created:   time.Now(),
	}
	if _, err := s.Table("tonno_eventsync_mappings").Insert(&mapping); err != nil {
		s.Rollback()
		return fmt.Errorf("failed to insert mapping: %w", err)
	}

	var templateProjID int64
	if config.TemplateProjectID > 0 {
		templateProjID = config.TemplateProjectID
	} else {
		var p Project
		has, err := s.Table("projects").Where("title = ?", config.TemplateProjectName).Get(&p)
		if err == nil && has {
			templateProjID = p.ID
			log.Infof("[tonno-eventsync] Progetto Modello trovato con ID: %d", templateProjID)
		} else {
			log.Infof("[tonno-eventsync] [WARN] Modello '%s' non trovato.", config.TemplateProjectName)
		}
	}

	if templateProjID > 0 {
		var templateViews []ProjectView
		if err := s.Table("project_views").Where("project_id = ?", templateProjID).Find(&templateViews); err != nil {
			s.Rollback()
			return fmt.Errorf("failed to fetch template views: %w", err)
		}

		bucketMap := make(map[int64]int64)

		for _, tView := range templateViews {
			newView := ProjectView{
				ProjectID: newProj.ID,
				Title:     tView.Title,
				ViewKind:  tView.ViewKind,
				Position:  tView.Position,
				Created:   time.Now(),
				Updated:   time.Now(),
			}
			if _, err := s.Table("project_views").Insert(&newView); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to insert new view: %w", err)
			}

			var templateBuckets []Bucket
			if err := s.Table("buckets").Where("project_view_id = ?", tView.ID).Asc("position").Find(&templateBuckets); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to fetch buckets: %w", err)
			}

			for _, tBucket := range templateBuckets {
				newBucket := Bucket{
					Title:         tBucket.Title,
					ProjectViewID: newView.ID,
					Position:      tBucket.Position,
					CreatedByID:   ownerID,    // <--- AGGIUNGI QUESTA
					Created:       time.Now(),
					Updated:       time.Now(),
				}
				if _, err := s.Table("buckets").Insert(&newBucket); err != nil {
					s.Rollback()
					return fmt.Errorf("failed to insert cloned bucket: %w", err)
				}
				bucketMap[tBucket.ID] = newBucket.ID
			}
		}

		var templateTasks []Task
		if err := s.Table("tasks").Where("project_id = ?", templateProjID).Find(&templateTasks); err != nil {
			s.Rollback()
			return fmt.Errorf("failed to fetch template tasks: %w", err)
		}

		log.Infof("[tonno-eventsync] Creazione di %d task clonati dal modello.", len(templateTasks))

		for _, tTask := range templateTasks {
			var mappedBucketID int64
			if tTask.BucketID > 0 {
				mappedBucketID = bucketMap[tTask.BucketID]
			}

			clonedTask := Task{
				Title:       tTask.Title,
				Description: tTask.Description,
				Priority:    tTask.Priority,
				ProjectID:   newProj.ID,
				BucketID:    mappedBucketID,
				Created:     time.Now(),
				Updated:     time.Now(),
			}
			if _, err := s.Table("tasks").Insert(&clonedTask); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to insert cloned task: %w", err)
			}

			relSub := TaskRelation{
				TaskID:       event.Task.ID,
				OtherTaskID:  clonedTask.ID,
				RelationKind: "subtask",
			}
			if _, err := s.Table("task_relations").Insert(&relSub); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to link subtask relation: %w", err)
			}

			relParent := TaskRelation{
				TaskID:       clonedTask.ID,
				OtherTaskID:  event.Task.ID,
				RelationKind: "parenttask",
			}
			if _, err := s.Table("task_relations").Insert(&relParent); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to link parenttask relation: %w", err)
			}
		}
		log.Infof("[tonno-eventsync] Sincronizzazione gerarchica completata!")
	}

	return s.Commit()
}

func (l *TaskCreatedListener) Name() string { return "tonno-eventsync-task-created" }

type TaskUpdatedListener struct{}

func (l *TaskUpdatedListener) Handle(msg *message.Message) error {
	var event models.TaskUpdatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		return err
	}
	if event.Task == nil {
		return nil
	}

	s := db.NewSession()
	defer s.Close()
	if err := s.Begin(); err != nil {
		return err
	}

	var mapping EventSyncMapping
	has, err := s.Table("tonno_eventsync_mappings").Where("task_id = ?", event.Task.ID).Get(&mapping)
	if err != nil || !has {
		s.Rollback()
		return err
	}

	var proj Project
	hasProj, err := s.Table("projects").Where("id = ?", mapping.ProjectID).Get(&proj)
	if err != nil {
		s.Rollback()
		return err
	}

	if hasProj && proj.Title != event.Task.Title {
		projUpdate := struct {
			Title string `xorm:"title"`
		}{Title: event.Task.Title}
		if _, err = s.Table("projects").Where("id = ?", proj.ID).Cols("title").Update(&projUpdate); err != nil {
			s.Rollback()
			return err
		}
		proj.Title = event.Task.Title
	}

	if event.Task.BucketID > 0 {
		type BucketCheck struct {
			ID    int64  `xorm:"id"`
			Title string `xorm:"title"`
		}
		var b BucketCheck
		if hasBucket, _ := s.Table("buckets").Where("id = ?", event.Task.BucketID).Get(&b); hasBucket && strings.EqualFold(b.Title, config.ConcludedColumnName) {
			if hasProj && !proj.IsArchived {
				projUpdate := struct {
					IsArchived bool `xorm:"is_archived"`
				}{IsArchived: true}
				if _, err = s.Table("projects").Where("id = ?", proj.ID).Cols("is_archived").Update(&projUpdate); err != nil {
					s.Rollback()
					return err
				}
			}
		}
	}

	return s.Commit()
}

func (l *TaskUpdatedListener) Name() string { return "tonno-eventsync-task-updated" }

type ProjectUpdatedListener struct{}

func (l *ProjectUpdatedListener) Handle(msg *message.Message) error {
	var event struct {
		Project *models.Project `json:"project"`
	}
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		return err
	}
	if event.Project == nil {
		return nil
	}

	s := db.NewSession()
	defer s.Close()
	if err := s.Begin(); err != nil {
		return err
	}

	var mapping EventSyncMapping
	if has, err := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Project.ID).Get(&mapping); err != nil || !has {
		s.Rollback()
		return err
	}

	var task Task
	if hasTask, err := s.Table("tasks").Where("id = ?", mapping.TaskID).Get(&task); err != nil || !hasTask {
		s.Rollback()
		return err
	}

	if task.Title != event.Project.Title {
		taskUpdate := struct {
			Title string `xorm:"title"`
		}{Title: event.Project.Title}
		if _, err := s.Table("tasks").Where("id = ?", task.ID).Cols("title").Update(&taskUpdate); err != nil {
			s.Rollback()
			return err
		}
	}

	return s.Commit()
}

func (l *ProjectUpdatedListener) Name() string { return "tonno-eventsync-project-updated" }

type TaskAssigneeCreatedListener struct{}

func (l *TaskAssigneeCreatedListener) Handle(msg *message.Message) error {
	var event models.TaskAssigneeCreatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		return err
	}
	if event.Task == nil || event.Assignee == nil {
		return nil
	}

	s := db.NewSession()
	defer s.Close()
	if err := s.Begin(); err != nil {
		return err
	}

	var mapping EventSyncMapping
	if has, err := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Task.ProjectID).Get(&mapping); err != nil || !has {
		s.Rollback()
		return err
	}

	exists, err := s.Table("task_assignees").Where("task_id = ? AND user_id = ?", mapping.TaskID, event.Assignee.ID).Exist()
	if err != nil {
		s.Rollback()
		return err
	}

	if !exists {
		newAssignee := TaskAssignee{
			TaskID:  mapping.TaskID,
			UserID:  event.Assignee.ID,
			Created: time.Now(),
		}
		if _, err = s.Table("task_assignees").Insert(&newAssignee); err != nil {
			s.Rollback()
			return err
		}
	}

	return s.Commit()
}

func (l *TaskAssigneeCreatedListener) Name() string { return "tonno-eventsync-task-assignee-created" }

func getUniqueProjectIdentifier() (string, error) {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 10; i++ {
		b := make([]byte, 6)
		for j := range b {
			b[j] = letters[r.Intn(len(letters))]
		}
		idStr := string(b)
		s := db.NewSession()
		exists, err := s.Table("projects").Where("identifier = ?", idStr).Exist()
		s.Close()
		if err != nil {
			return "", err
		}
		if !exists {
			return idStr, nil
		}
	}
	return "", fmt.Errorf("failed to generate identifier")
}
