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

// Config holds the configuration settings loaded from environment variables.
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

// EventSyncMapping rappresenta la mappatura tra un task padre e il suo sotto-progetto operativo.
type EventSyncMapping struct {
	TaskID    int64     `xorm:"pk"`
	ProjectID int64     `xorm:"unique"`
	Created   time.Time `xorm:"created"`
}

func (EventSyncMapping) TableName() string {
	return "tonno_eventsync_mappings"
}

// Project rappresenta i campi reali della tabella projects di Vikunja.
type Project struct {
	ID              int64     `xorm:"bigint autoincr pk"`
	Title           string    `xorm:"varchar(250) not null"`
	Description     string    `xorm:"text"`
	OwnerID         int64     `xorm:"bigint"`
	ParentProjectID int64     `xorm:"bigint"`
	Identifier      string    `xorm:"varchar(250)"`
	IsArchived      bool      `xorm:"not null default false"`
	Created         time.Time `xorm:"created"`
	Updated         time.Time `xorm:"updated"`
}

func (Project) TableName() string {
	return "projects"
}

// Task rappresenta i campi reali della tabella tasks di Vikunja (Rimosso parent_task_id che rompeva la query).
type Task struct {
	ID          int64     `xorm:"bigint autoincr pk"`
	Title       string    `xorm:"text not null"`
	Description string    `xorm:"text"`
	Done        bool      `xorm:"index null"`
	DoneAt      time.Time `xorm:"index null"`
	Priority    int64     `xorm:"bigint"`
	ProjectID   int64     `xorm:"bigint not null"`
	Created     time.Time `xorm:"created"`
	Updated     time.Time `xorm:"updated"`
}

func (Task) TableName() string {
	return "tasks"
}

// TaskRelation gestisce i collegamenti e i sotto-task in Vikunja.
type TaskRelation struct {
	ID           int64  `xorm:"bigint autoincr pk"`
	TaskID       int64  `xorm:"bigint not null"`
	OtherTaskID  int64  `xorm:"bigint not null"`
	RelationKind string `xorm:"varchar(255) not null"`
}

func (TaskRelation) TableName() string {
	return "task_relations"
}

// TaskAssignee rappresenta le assegnazioni degli utenti ai task.
type TaskAssignee struct {
	TaskID  int64     `xorm:"not null"`
	UserID  int64     `xorm:"not null"`
	Created time.Time `xorm:"created"`
}

func (TaskAssignee) TableName() string {
	return "task_assignees"
}

type TonnoEventSyncPlugin struct{}

var (
	_ plugins.Plugin = (*TonnoEventSyncPlugin)(nil)
)

var singleton = &TonnoEventSyncPlugin{}

func NewPlugin() plugins.Plugin {
	return singleton
}

func (p *TonnoEventSyncPlugin) Name() string    { return "tonno-eventsync" }
func (p *TonnoEventSyncPlugin) Version() string { return "1.0.0" }

func (p *TonnoEventSyncPlugin) Init() error {
	log.Infof("Initializing %s version %s", p.Name(), p.Version())
	
	s := db.NewSession()
	defer s.Close()
	
	if err := s.Begin(); err != nil {
		log.Errorf("[tonno-eventsync] Failed to begin database transaction: %v", err)
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
		log.Errorf("[tonno-eventsync] Failed to create database table via raw SQL: %v", err)
		s.Rollback()
		return err
	}
	
	if err := s.Commit(); err != nil {
		log.Errorf("[tonno-eventsync] Failed to commit database transaction: %v", err)
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
	
	if macroProjID == 0 {
		s.Rollback()
		return nil
	}
	
	if event.Task.ProjectID != macroProjID {
		s.Rollback()
		return nil
	}
	
	if event.Task.BucketID > 0 {
		type Bucket struct {
			ID    int64  `xorm:"id"`
			Title string `xorm:"title"`
		}
		var b Bucket
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
	if err != nil {
		s.Rollback()
		return err
	}
	if hasMap {
		s.Rollback()
		return nil
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
			log.Infof("[tonno-eventsync] Progetto Modello trovato con ID database: %d", templateProjID)
		} else {
			log.Infof("[tonno-eventsync] [WARN] Impossibile trovare un progetto modello chiamato esattamente '%s'.", config.TemplateProjectName)
		}
	}
	
	if templateProjID > 0 {
		var templateTasks []Task
		if err := s.Table("tasks").Where("project_id = ?", templateProjID).Find(&templateTasks); err != nil {
			s.Rollback()
			return fmt.Errorf("failed to fetch template tasks: %w", err)
		}
		
		log.Infof("[tonno-eventsync] Trovati %d task da clonare dal modello.", len(templateTasks))
		
		for _, tTask := range templateTasks {
			clonedTask := Task{
				Title:       tTask.Title,
				Description: tTask.Description,
				Priority:    tTask.Priority,
				ProjectID:   newProj.ID, // Inserito nel nuovo sotto-progetto
				Created:     time.Now(),
				Updated:     time.Now(),
			}
			if _, err := s.Table("tasks").Insert(&clonedTask); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to insert cloned task: %w", err)
			}
			
			// 🔗 SCRITTURA DELLA RELAZIONE SOTTO-TASK NELLA TABELLA DI COLLEGAMENTO
			// Relazione 1: La Card Madre dichiara che questo task clonato è un suo "subtask"
			relSub := TaskRelation{
				TaskID:       event.Task.ID,
				OtherTaskID:  clonedTask.ID,
				RelationKind: "subtask",
			}
			if _, err := s.Table("task_relations").Insert(&relSub); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to link subtask relation: %w", err)
			}
			
			// Relazione 2: Il task clonato dichiara che la Card Madre è il suo "parenttask"
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
		log.Infof("[tonno-eventsync] Clonazione completata e sotto-task collegati alla card madre via task_relations.")
	}
	
	return s.Commit()
}

func (l *TaskCreatedListener) Name() string {
	return "tonno-eventsync-task-created"
}

type TaskUpdatedListener struct{}

func (l *TaskUpdatedListener) Handle(msg *message.Message) error {
	var event models.TaskUpdatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		log.Errorf("[tonno-eventsync] Failed to unmarshal TaskUpdatedEvent: %v", err)
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
	if err != nil {
		s.Rollback()
		return err
	}
	
	if !has {
		s.Rollback()
		return nil
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
		}{
			Title: event.Task.Title,
		}
		_, err = s.Table("projects").Where("id = ?", proj.ID).Cols("title").Update(&projUpdate)
		if err != nil {
			s.Rollback()
			return fmt.Errorf("failed to update project title: %w", err)
		}
		log.Infof("[tonno-eventsync] Synchronized project title to '%s'", event.Task.Title)
		proj.Title = event.Task.Title
	}
	
	if event.Task.BucketID > 0 {
		type Bucket struct {
			ID    int64  `xorm:"id"`
			Title string `xorm:"title"`
		}
		var b Bucket
		hasBucket, err := s.Table("buckets").Where("id = ?", event.Task.BucketID).Get(&b)
		if err != nil {
			s.Rollback()
			return err
		}
		if hasBucket && strings.EqualFold(b.Title, config.ConcludedColumnName) {
			if hasProj && !proj.IsArchived {
				projUpdate := struct {
					IsArchived bool `xorm:"is_archived"`
				}{
					IsArchived: true,
				}
				_, err = s.Table("projects").Where("id = ?", proj.ID).Cols("is_archived").Update(&projUpdate)
				if err != nil {
					s.Rollback()
					return fmt.Errorf("failed to archive project: %w", err)
				}
				log.Infof("[tonno-eventsync] Archived project '%s' as task moved to '%s'", proj.Title, b.Title)
			}
		}
	}
	
	return s.Commit()
}

func (l *TaskUpdatedListener) Name() string {
	return "tonno-eventsync-task-updated"
}

type ProjectUpdatedListener struct{}

func (l *ProjectUpdatedListener) Handle(msg *message.Message) error {
	var event struct {
		Project *models.Project `json:"project"`
	}
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		log.Errorf("[tonno-eventsync] Failed to unmarshal ProjectUpdatedEvent: %v", err)
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
	has, err := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Project.ID).Get(&mapping)
	if err != nil {
		s.Rollback()
		return err
	}
	
	if !has {
		s.Rollback()
		return nil
	}
	
	var task Task
	hasTask, err := s.Table("tasks").Where("id = ?", mapping.TaskID).Get(&task)
	if err != nil {
		s.Rollback()
		return err
	}
	
	if hasTask && task.Title != event.Project.Title {
		taskUpdate := struct {
			Title string `xorm:"title"`
		}{
			Title: event.Project.Title,
		}
		_, err = s.Table("tasks").Where("id = ?", task.ID).Cols("title").Update(&taskUpdate)
		if err != nil {
			s.Rollback()
			return fmt.Errorf("failed to update task title: %w", err)
		}
		log.Infof("[tonno-eventsync] Synchronized task title to '%s'", event.Project.Title)
	}
	
	return s.Commit()
}

func (l *ProjectUpdatedListener) Name() string {
	return "tonno-eventsync-project-updated"
}

type TaskAssigneeCreatedListener struct{}

func (l *TaskAssigneeCreatedListener) Handle(msg *message.Message) error {
	var event models.TaskAssigneeCreatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		log.Errorf("[tonno-eventsync] Failed to unmarshal TaskAssigneeCreatedEvent: %v", err)
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
	has, err := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Task.ProjectID).Get(&mapping)
	if err != nil {
		s.Rollback()
		return err
	}
	
	if !has {
		s.Rollback()
		return nil
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
		_, err = s.Table("task_assignees").Insert(&newAssignee)
		if err != nil {
			s.Rollback()
			return fmt.Errorf("failed to assign user to parent task: %w", err)
		}
		log.Infof("[tonno-eventsync] Assigned user %d to parent task %d", event.Assignee.ID, mapping.TaskID)
	}
	
	return s.Commit()
}

func (l *TaskAssigneeCreatedListener) Name() string {
	return "tonno-eventsync-task-assignee-created"
}

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
	return "", fmt.Errorf("failed to generate unique project identifier after 10 attempts")
}