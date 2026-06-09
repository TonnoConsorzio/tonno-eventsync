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
	CreatedByID   int64     `xorm:"created_by_id not null"` // Ripristinato per evitare not-null constraint
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
	PercentDone float64   `xorm:"percent_done"`
	Priority    int64     `xorm:"index"`
	ProjectID   int64     `xorm:"not null index"`
	CreatedByID int64     `xorm:"created_by_id not null"`
	Created     time.Time `xorm:"created"`
	Updated     time.Time `xorm:"updated"`
}

func (Task) TableName() string { return "tasks" }

type TaskRelation struct {
	ID           int64     `xorm:"pk autoincr"`
	TaskID       int64     `xorm:"not null index"`
	OtherTaskID  int64     `xorm:"not null index"`
	RelationKind string    `xorm:"varchar(255) not null"`
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

	go runRecoverySync()

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

	if macroProjID == 0 {
		s.Rollback()
		return nil
	}

	// 1. GESTIONE TASK CREATI DIRETTAMENTE NEL SOTTOPROGETTO
	if event.Task.ProjectID != macroProjID {
		var mapping EventSyncMapping
		hasMap, err := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Task.ProjectID).Get(&mapping)
		if err == nil && hasMap {
			var ownerID int64
			if event.Doer != nil && event.Doer.ID > 0 {
				ownerID = event.Doer.ID
			} else {
				ownerID = 1
			}

			// Collega come subtask
			relSub := TaskRelation{
				TaskID:       mapping.TaskID,
				OtherTaskID:  event.Task.ID,
				RelationKind: "subtask",
			}
			s.Table("task_relations").Insert(&relSub)

			relParent := TaskRelation{
				TaskID:       event.Task.ID,
				OtherTaskID:  mapping.TaskID,
				RelationKind: "parenttask",
			}
			s.Table("task_relations").Insert(&relParent)

			// Ricalcola completamento
			var allTasks []Task
			if errTasks := s.Table("tasks").Where("project_id = ?", event.Task.ProjectID).Find(&allTasks); errTasks == nil {
				var percentDone float64
				if len(allTasks) > 0 {
					var doneCount int
					for _, t := range allTasks {
						if t.Done || t.PercentDone >= 1 {
							doneCount++
						}
					}
					percentDone = float64(doneCount) / float64(len(allTasks))
				}
				type TaskPercentUpdate struct {
					PercentDone float64 `xorm:"percent_done"`
				}
				s.Table("tasks").Where("id = ?", mapping.TaskID).Cols("percent_done").Update(&TaskPercentUpdate{PercentDone: percentDone})
			}

			return s.Commit()
		}

		s.Rollback()
		return nil
	}

	// 2. GESTIONE TASK CREATO NEL PROGETTO PRINCIPALE (Clonazione)
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
		if err := s.SQL("SELECT id, project_id, title, view_kind, position, created, updated FROM project_views WHERE project_id = ?", templateProjID).Find(&templateViews); err != nil {
			s.Rollback()
			return fmt.Errorf("failed to fetch template views: %w", err)
		}

		bucketMap := make(map[int64]int64)

		for _, tView := range templateViews {
			newView := ProjectView{
				ProjectID:   newProj.ID,
				Title:       tView.Title,
				ViewKind:    tView.ViewKind,
				Position:    tView.Position,
				Created:     time.Now(),
				Updated:     time.Now(),
			}
			if _, err := s.Table("project_views").Insert(&newView); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to insert new view: %w", err)
			}

			var templateBuckets []Bucket
			if err := s.SQL("SELECT id, title, project_view_id, position, created, updated FROM buckets WHERE project_view_id = $1 ORDER BY position ASC", tView.ID).Find(&templateBuckets); err != nil {
				s.Rollback()
				return fmt.Errorf("failed to fetch buckets: %w", err)
			}

			for _, tBucket := range templateBuckets {
				newBucket := Bucket{
					Title:         tBucket.Title,
					ProjectViewID: newView.ID,
					Position:      tBucket.Position,
					CreatedByID:   ownerID,
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
			clonedTask := Task{
				Title:       tTask.Title,
				Description: tTask.Description,
				Priority:    tTask.Priority,
				ProjectID:   newProj.ID,
				CreatedByID: ownerID, // <--- AGGIUNGI QUESTA
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
		// Controllo se è un task DEL sottoprogetto (aggiornamento % completamento)
		hasSub, errSub := s.Table("tonno_eventsync_mappings").Where("project_id = ?", event.Task.ProjectID).Get(&mapping)
		if errSub == nil && hasSub {
			// Ricalcola completamento
			var allTasks []Task
			if errTasks := s.Table("tasks").Where("project_id = ?", event.Task.ProjectID).Find(&allTasks); errTasks == nil {
				var percentDone float64
				if len(allTasks) > 0 {
					var doneCount int
					for _, t := range allTasks {
						if t.Done || t.PercentDone >= 1 {
							doneCount++
						}
					}
					percentDone = float64(doneCount) / float64(len(allTasks))
				}
				type TaskPercentUpdate struct {
					PercentDone float64 `xorm:"percent_done"`
				}
				s.Table("tasks").Where("id = ?", mapping.TaskID).Cols("percent_done").Update(&TaskPercentUpdate{PercentDone: percentDone})
			}
			return s.Commit()
		}

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

func runRecoverySync() {
	time.Sleep(5 * time.Second) // Attendi che Vikunja sia avviato completamente

	log.Infof("[tonno-eventsync] Starting recovery mega-sync...")

	s := db.NewSession()
	defer s.Close()

	var macroProjID int64
	if config.MacroProjectID > 0 {
		macroProjID = config.MacroProjectID
	} else {
		var p Project
		has, err := s.Table("projects").Where("title = ?", config.MacroProjectName).Get(&p)
		if err != nil || !has {
			log.Errorf("[tonno-eventsync] Recovery sync failed: MacroProject not found")
			return
		}
		macroProjID = p.ID
	}

	var mainTasks []Task
	if err := s.Table("tasks").Where("project_id = ?", macroProjID).Find(&mainTasks); err != nil {
		log.Errorf("[tonno-eventsync] Recovery sync failed to fetch main tasks: %v", err)
		return
	}

	for _, mainTask := range mainTasks {
		var mapping EventSyncMapping
		hasMap, err := s.Table("tonno_eventsync_mappings").Where("task_id = ?", mainTask.ID).Get(&mapping)
		
		var subProjectID int64

		if err == nil && hasMap {
			subProjectID = mapping.ProjectID
		} else {
			// Cerchiamo un progetto figlio con lo stesso titolo
			var subProj Project
			hasSub, errSub := s.Table("projects").
				Where("parent_project_id = ? AND title = ?", macroProjID, mainTask.Title).
				Get(&subProj)
			
			if errSub == nil && hasSub {
				subProjectID = subProj.ID
				
				// Creiamo il mapping mancante
				newMapping := EventSyncMapping{
					TaskID:    mainTask.ID,
					ProjectID: subProjectID,
					Created:   time.Now(),
				}
				s.Table("tonno_eventsync_mappings").Insert(&newMapping)
				log.Infof("[tonno-eventsync] Restored missing mapping for task '%s'", mainTask.Title)
			}
		}

		if subProjectID > 0 {
			var subTasks []Task
			if err := s.Table("tasks").Where("project_id = ?", subProjectID).Find(&subTasks); err != nil {
				continue
			}

			// Otteniamo il creatore della main task come default per i link
			ownerID := int64(1)
			if mainTask.CreatedByID > 0 {
				ownerID = mainTask.CreatedByID
			}

			for _, subT := range subTasks {
				// Verifica relazione subtask
				existSub, _ := s.Table("task_relations").
					Where("task_id = ? AND other_task_id = ? AND relation_kind = ?", mainTask.ID, subT.ID, "subtask").
					Exist()
				
				if !existSub {
					relSub := TaskRelation{
						TaskID:       mainTask.ID,
						OtherTaskID:  subT.ID,
						RelationKind: "subtask",
					}
					s.Table("task_relations").Insert(&relSub)
					log.Infof("[tonno-eventsync] Restored subtask relation for '%s'", subT.Title)
				}

				// Verifica relazione parenttask
				existParent, _ := s.Table("task_relations").
					Where("task_id = ? AND other_task_id = ? AND relation_kind = ?", subT.ID, mainTask.ID, "parenttask").
					Exist()

				if !existParent {
					relParent := TaskRelation{
						TaskID:       subT.ID,
						OtherTaskID:  mainTask.ID,
						RelationKind: "parenttask",
					}
					s.Table("task_relations").Insert(&relParent)
				}
			}

			// Infine, ricalcoliamo il completamento per sistemarlo!
			var allSubTasks []Task
			if errTasks := s.Table("tasks").Where("project_id = ?", subProjectID).Find(&allSubTasks); errTasks == nil {
				var percentDone float64
				if len(allSubTasks) > 0 {
					var doneCount int
					for _, t := range allSubTasks {
						if t.Done || t.PercentDone >= 1 {
							doneCount++
						}
					}
					percentDone = float64(doneCount) / float64(len(allSubTasks))
				}
				type TaskPercentUpdate struct {
					PercentDone float64 `xorm:"percent_done"`
				}
				s.Table("tasks").Where("id = ?", mainTask.ID).Cols("percent_done").Update(&TaskPercentUpdate{PercentDone: percentDone})
			}
		}
	}

	log.Infof("[tonno-eventsync] Recovery mega-sync completed!")
}
