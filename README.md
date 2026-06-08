# Vikunja Event Sync Plugin

Plugin di backend per Vikunja (v2.3.0) sviluppato per **ABBO APS** per la sincronizzazione dinamica bi-direzionale tra la bacheca vision globale (Kanban) e l'area operativa di dettaglio (Sotto-progetti).

---

## 🧭 Architettura dell'Integrazione

Il plugin gestisce il ciclo di vita degli eventi sincronizzando due mondi:
1. **La Card (Bacheca Kanban)**: Un task all'interno del macro-progetto `🎡 Eventi sul Territorio` (o altro configurato). Rappresenta lo stato globale, gestisce date e assegnatari complessivi.
2. **Il Sotto-progetto (Area di Lavoro)**: Un sotto-progetto dedicato nella barra laterale che racchiude tutte le attività atomiche per quell'evento.

La relazione bi-direzionale viene mappata in database tramite la tabella custom `tonno_eventsync_mappings`.

---

## 🔄 Flussi di Automazione

### 🔹 FLUSSO A: Inizializzazione e Sincronizzazione dell'Identità (Nome)
* **Trigger**: Creazione di un task nella colonna/bucket `Nuovi Eventi` del macro-progetto.
* **Azione**:
  1. Il sistema rileva il task e crea istantaneamente un Sotto-progetto con lo stesso titolo.
  2. Associa il Sotto-progetto al macro-progetto impostando l'ID del macro-progetto come parent.
  3. Cerca il progetto modello `Modello Standard per gli Eventi` (o configurato) e clona tutti i suoi task operativi nel nuovo sotto-progetto.
  4. Crea relazioni speculari (`subtask` / `parenttask`) tra la Card principale e i singoli task clonati nel sotto-progetto.

#### Sincronizzazione Nomi Bi-direzionale:
* Se viene rinominato il Task (Card Kanban), il Sotto-progetto corrispondente viene rinominato istantaneamente.
* Se viene rinominato il Sotto-progetto, il Task (Card Kanban) associato viene rinominato di conseguenza.
* *Nota: Il sistema include un meccanismo di controllo preventivo dello stato per bloccare loop ricorsivi infiniti di aggiornamento.*

### 🔹 FLUSSO B: Motore delle Sotto-attività e Percentuale Dinamica
* Grazie alle relazioni `subtask` inserite all'inizio, ogni spunta o completamento di un task all'interno del sotto-progetto aggiorna istantaneamente lo stato del sotto-task associato alla Card principale.
* La barra di avanzamento e i conteggi sulla bacheca Kanban mostreranno in tempo reale il reale progresso operativo (es. 1/40 o 2.5%).

### 🔹 FLUSSO C: Assegnazione Dinamica dei Collaboratori (Chi fa cosa)
* Quando un collaboratore viene assegnato a un task operativo del sotto-progetto:
  * Il sistema controlla se l'utente è già tra gli assegnatari della Card madre Kanban.
  * Se non lo è, lo aggiunge automaticamente per mostrare l'avatar del collaboratore sul tabellone generale.

### 📈 Chiusura Evento (Fine Ciclo)
* Quando l'evento giunge al termine e la Card principale viene spostata nella colonna `Eventi Conclusi`:
  * Il sotto-progetto operativo viene automaticamente archiviato (`is_archived = true`), scomparendo dalla barra laterale giornaliera.
  * I dati del sotto-progetto e la Card Kanban rimangono intatti nella cronologia per consultazione futura.

---

## ⚙️ Configurazione

Il plugin non necessita di file di configurazione statici ed è controllabile tramite le seguenti variabili d'ambiente (ideale per installazioni Docker/Docker Compose):

| Variabile d'Ambiente | Descrizione | Default |
| :--- | :--- | :--- |
| `VIKUNJA_EVENT_MACRO_PROJECT_NAME` | Nome del macro-progetto Kanban globale degli eventi. | `"🎡 Eventi sul Territorio"` |
| `VIKUNJA_EVENT_MACRO_PROJECT_ID` | ID diretto del macro-progetto (sovrascrive la ricerca per nome). | |
| `VIKUNJA_EVENT_NEW_COLUMN_NAME` | Nome della colonna Kanban per l'inizializzazione. | `"Nuovi Eventi"` |
| `VIKUNJA_EVENT_CONCLUDED_COLUMN_NAME` | Nome della colonna Kanban per l'archiviazione di fine ciclo. | `"Eventi Conclusi"` |
| `VIKUNJA_EVENT_TEMPLATE_PROJECT_NAME` | Nome del progetto da clonare come template. | `"Modello Standard per gli Eventi"` |
| `VIKUNJA_EVENT_TEMPLATE_PROJECT_ID` | ID diretto del progetto template (sovrascrive la ricerca per nome). | |

---

## 🛠️ Struttura Database Mappata
Il plugin applica all'avvio la creazione della seguente tabella PostgreSQL:
* **Tabella**: `tonno_eventsync_mappings`
  * `task_id` (bigint, PK, Unique, Not Null) - ID del Task principale.
  * `project_id` (bigint, Unique, Not Null) - ID del Sotto-progetto associato.
  * `created` (timestamp) - Data di creazione del collegamento.

---

## 📜 Licenza
Questo progetto è protetto da licenza MIT.
# tonno-eventsync
