# Architettura d'Integrazione e Specifica Operativa AGENT.md per lo Sviluppo di Plugin in Vikunja

L'estendibilità del backend di Vikunja costituisce un pilastro fondamentale per l'adattamento dell'applicazione a flussi di lavoro aziendali complessi, consentendo l'aggiunta di funzionalità personalizzate senza alterare il nucleo del codice sorgente. Per massimizzare l'efficacia di assistenti di intelligenza artificiale basati su modelli linguistici avanzati come Claude e Gemini, è necessario strutturare un protocollo di istruzioni operativo chiaro, formalizzato all'interno di un file denominato `AGENT.md`.

Questo rapporto tecnico delinea l'architettura dei servizi di Vikunja, confronta i motori di caricamento dei plugin, analizza le limitazioni dell'interprete integrato e fornisce la specifica completa del file `AGENT.md` tarata sull'ambiente di produzione (Vikunja 2.3.0, PostgreSQL, Watermill Events).

---

## Architettura Backend di Vikunja e Contesto delle Estensioni

Il backend di Vikunja (v2.3.0), sviluppato in linguaggio Go, adotta un'architettura stratificata che separa rigorosamente la rappresentazione dei dati dalla logica di business e dalle interfacce di comunicazione HTTP.

### Struttura dei Livelli Core del Backend
* **Models (`pkg/models/`)**: Gestiscono la definizione delle entità di dominio, le operazioni CRUD fondamentali e l'applicazione delle regole di autorizzazione e sicurezza a livello di record.
* **Services (`pkg/services/`)**: Coordinano i flussi di business complessi e le interazioni che orchestrano più modelli o sistemi esterni.
* **Routes (`pkg/routes/`)**: Definiscono gli endpoint HTTP, la mappatura dei parametri e la configurazione del routing API.
* **Web (`pkg/web/`)**: Forniscono astrazioni generiche per la gestione delle richieste e l'interfaccia con il framework web.

Un'importante evoluzione architetturale riguarda la transizione del backend verso l'uso del framework **Huma** per la gestione delle API. Attualmente, lo sviluppo di nuove funzionalità nel core di Vikunja deve indirizzarsi tassativamente verso lo spazio `/api/v2` (collocato in `pkg/routes/api/v2/`), mentre lo spazio `/api/v1` è da considerarsi congelato e deputato esclusivamente alla manutenzione correttiva.

> **Nota di deroga**: I plugin operano in una deroga parziale a questa transizione: le rotte registrate dalle estensioni esterne vengono esposte sotto i prefissi dedicati `/api/v1/plugins/` per gli endpoint autenticati e `/plugins/` per quelli pubblici.

La persistenza dei dati è affidata all'ORM **XORM**, configurato in produzione su **PostgreSQL**. La gestione dei cambiamenti strutturali del database è governata da un sistema di migrazione basato sul pacchetto `xormigrate`. Ogni modulo o plugin che introduce nuove strutture dati deve implementare schemi transazionali autosufficienti per garantire l'integrità del database all'avvio dell'applicazione o durante gli aggiornamenti. L'uso di PostgreSQL permette l'utilizzo nativo di vincoli relazionali complessi e indici avanzati (es. `Partial Indexes`) senza le limitazioni tipiche di SQLite.

---

## Analisi Comparativa dei Loader di Plugin

Vikunja integra storicamente due diversi meccanismi per il caricamento e l'esecuzione di codice esterno. La scelta del loader determina i vincoli di compilazione, i requisiti di sistema e la struttura stessa del codice del plugin.

| Parametro Ingegneristico | Loader Yaegi (Raccomandato) | Loader Nativo (Deprecato) |
| :--- | :--- | :--- |
| **Meccanismo Operativo** | Interpretazione dinamica dell'Abstract Syntax Tree (AST) dei file sorgente Go a runtime. | Caricamento dinamico in memoria di librerie condivise compilate con estensione `.so`. |
| **Requisiti di Compilazione** | Nessuno. I sorgenti in chiaro vengono distribuiti direttamente nella directory dei plugin. | Rigidi. Compilazione obbligatoria con l'esatta versione di Go e delle dipendenze del binario Vikunja. |
| **Compatibilità OS** | Completa portabilità cross-platform (inclusi sistemi Windows, macOS e Linux). | Limitata esclusivamente ad ambienti di tipo Unix (Linux e macOS). |
| **Rischio di Disallineamento** | Minimo, legato unicamente a modifiche strutturali nelle interfacce esportate dai pacchetti core. | Massimo. Qualsiasi minima discrepanza nelle dipendenze impedisce l'avvio del plugin o genera panico a runtime. |
| **Stato del Ciclo di Vita** | Supportato e attivamente sviluppato a partire dalla versione 2.3.0 di Vikunja. | Deprecato. Destinato alla completa rimozione nelle future release di sistema. |

### Limitazione dell'Interprete Yaegi
L'adozione di Yaegi introduce una limitazione critica che influenza la topologia del codice dei plugin. Dal momento che Yaegi valuta i file sorgente all'interno della cartella del plugin in modo atomico e sequenziale, la presenza di dichiarazioni incrociate suddivise su più file (es. `handlers.go`, `models.go`) può indurre errori sistematici di risoluzione dei simboli legati all'ordine alfabetico dei nomi dei file.

**Soluzione Ingegneristica**: Per aggirare questo limite dell'interprete, la prassi impone di consolidare l'intera implementazione del plugin all'interno di un unico file sorgente denominato `main.go`.

---

## Integrazione delle Interfacce e Lifecycle dei Plugin

L'integrazione di un plugin con il core di Vikunja avviene tramite l'implementazione di interfacce Go predefinitive, fornite dal modulo `github.com/vikunja/vikunja/pkg/plugins`.

| Nome Interfaccia | Firma del Metodo Richiesto | Ruolo e Comportamento nel Ciclo di Vita |
| :--- | :--- | :--- |
| **Plugin** | `PluginName() string`<br>`Version() string`<br>`Init() error`<br>`Shutdown() error` | Interfaccia base obbligatoria per ogni plugin. Gestisce l'identificazione, l'inizializzazione dello stato, la registrazione degli event listener asincroni e il rilascio controllato delle risorse. |
| **AuthenticatedRouterPlugin** | `RegisterAuthenticatedRoutes(g *echo.Group)` | Consente la registrazione di rotte protette da token JWT o chiavi API, mappate sotto il prefisso `/api/v1/plugins/{name}/`. |
| **UnauthenticatedRouterPlugin** | `RegisterUnauthenticatedRoutes(g *echo.Group)` | Consente l'esposizione di endpoint pubblici (es. webhook di integrazione esterni) accessibili liberamente sotto il percorso `/plugins/{name}/`. |
| **MigrationPlugin** | `Migrations() *xormigrate.Migration` | Permette la definizione di schemi di database personalizzati che vengono eseguiti automaticamente all'attivazione dell'estensione. |

> **Pattern d'Integrazione Consigliato**: Un aspetto cruciale per i plugin complessi, che implementano contemporaneamente capacità di routing, gestione eventi e migrazione dei dati, risiede nell'evitare l'istanziazione multipla della struttura logica del plugin. La duplicazione dello stato interno viene prevenuta efficacemente implementando il pattern di progettazione **Singleton**. In questo modo, tutte le funzioni factory esportate a livello di package restituiscono il medesimo riferimento di memoria della struttura del plugin.

---

## Specifica Strutturata del File AGENT.md per LLM

*Il blocco seguente definisce il protocollo operativo per l'assistente artificiale (Claude o Gemini). È progettato per essere interpretato direttamente dai modelli di IA.*

```markdown
# VIKUNJA BACKEND PLUGIN DEVELOPMENT CONVENTIONS

This document defines the strict operational boundaries, architecture rules, and validation processes for generating and modifying Vikunja backend plugins. The agent must adhere to these policies in every turn.

## 1. Architectural Rules and Structural Constraints

### Target Runtime & Single-File Constraint
* All plugin code must target the Yaegi runtime interpreter (Vikunja v2.3.0+). Native compiled plugins (.so) are deprecated and must not be generated.
* Due to sequential order-dependency limitations in the Yaegi AST interpreter, all plugin logic, structures, event handlers, and helper functions must be consolidated inside a single file named `main.go` within the plugin's directory. Do not split logic across multiple files unless explicitly instructed.

### Target Environment & Database Constraints
* **Target DB**: **PostgreSQL**. You can perform database operations (using `pkg/db` and XORM) to write/modify records if they are not persisted automatically by the event listeners. Ensure connections are closed properly.

### Code Layout and Design Patterns
* Utilize the **Singleton Pattern** for complex plugins implementing multiple interfaces. Declaring a package-level pointer variable ensures all factory constructors expose the identical memory instance.
* All Go errors must be wrapped static errors. Do not define dynamic errors on the fly; leverage `fmt.Errorf("context: %w", err)`.
* Comments must focus exclusively on documenting the non-obvious "why" (invariants, architectural workarounds, rejected designs). Do not write comments that restate the code, function names, or signatures.

### Core Libraries & Imports
Ensure imports target the exact major versions used by Vikunja:
* **Echo HTTP Framework**: `github.com/labstack/echo/v5`
* **Database Session & ORM**: `code.vikunja.io/api/pkg/db`, `xorm.io/xorm`, `src.techknowlogick.com/xormigrate`
* **Logging API**: `code.vikunja.io/api/pkg/log`
* **Core Interfaces**: `code.vikunja.io/api/pkg/plugins`
* **Asynchronous Events**: `code.vikunja.io/api/pkg/events`

## 2. Command-First Verification Suite

The agent must run the following exact verification commands to validate code correctness, dependency health, and compilation status before claiming any task as complete.

| Step | Scope of Verification | Command to Execute | Expected Outcome |
| :--- | :--- | :--- | :--- |
| 1 | Dependency Resolution | `go mod download` | Exit Code 0, no unresolved imports. |
| 2 | Syntax & Linter Rules | `golangci-lint run ./plugins/...` | Zero errors or warning outputs. |
| 3 | Alternate Core Linter | `mage lint:fix` | Automatic styling and code correction. |
| 4 | Runtime Verification | `docker logs <container-name> 2>&1 \| grep -i plugin` | Initialization string "initialized" printed in stdout. |

## 3. Reference Implementation Blueprint (main.go Template)

When generating a new plugin, use this robust, compiling template as the baseline structural blueprint, which includes PostgreSQL migration configurations and Watermill-driven event subscription handling.

```go
package main

import (
	"encoding/json"

	"github.com/ThreeDotsLabs/watermill/message"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"
)

// MyPlugin represents the concrete implementation of the plugin.
type MyPlugin struct{}

// Ensure compilation-time check for implemented interfaces
var (
	_ plugins.Plugin = (*MyPlugin)(nil)
)

// Declare the singleton instance to prevent multiple allocations.
var singleton = &MyPlugin{}

// Factory functions required by the Vikunja loader system.
func NewPlugin() plugins.Plugin {
	return singleton
}

// Name returns the unique, kebab-case identifier of the plugin.
func (p *MyPlugin) Name() string {
	return "my-extended-plugin"
}

// Version returns the semantic versioning representation.
func (p *MyPlugin) Version() string {
	return "1.0.0"
}

type MyTaskCreatedListener struct{}

func (l *MyTaskCreatedListener) Handle(msg *message.Message) error {
	var event models.TaskCreatedEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		return err
	}
	return handleTaskChange(event.Task, event.Doer)
}

func (l *MyTaskCreatedListener) Name() string {
	return "my-extended-task-created"
}

// Init handles event registrations.
func (p *MyPlugin) Init() error {
	log.Infof("Initializing %s version %s", p.Name(), p.Version())

	// Register listener using standard name and interface.
	events.RegisterListener("task.created", &MyTaskCreatedListener{})

	return nil
}

// Shutdown ensures clean teardown.
func (p *MyPlugin) Shutdown() error {
	log.Infof("Shutting down %s", p.Name())
	return nil
}

func handleTaskChange(task *models.Task, doer *user.User) error {
	if task == nil {
		return nil
	}
	
	// Modify task fields in DB or in memory
	if task.Priority == 0 {
		task.Priority = 3 // Fallback priority
	}
	return nil
}
```

## 4. Failure Recovery and Escalation Protocol

If the agent encounters errors during file generation or testing, it must follow this prioritized tree of actions instead of looping or applying brute-force edits:

1. **Compilation Failures**: Inspect the import block. Since Yaegi evaluates code on the fly, verify that no external, non-vendored package is included in the import list. If a dependency is missing, stop and report the dependency name.
2. **Database Migration Failures**: Verify PostgreSQL schema rules. Ensure types used inside `xorm:"..."` tags match standard PostgreSQL conventions.
3. **Event Registration Failures**: Verify event names against the core Vikunja event list. If an event string is invalid, the register chain will panic at boot time.
4. **Escalation Trigger**: If a bug is not resolved after 3 consecutive testing cycles, the agent must output the current state of `main.go`, the full CLI error logs, and wait for human intervention. Never silently bypass linters, delete code files, or skip authentication boundaries to make the code pass.
```