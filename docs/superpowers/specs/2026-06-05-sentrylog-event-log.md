# SentryLog · Event Log + Access Analyzer — Design Spec

**Fecha:** 2026-06-05
**Estado:** Aprobado, pendiente de plan de implementación.
**Contexto:** Cierra la convergencia I+D↔producto ([[access-driven-rd-indexing]]): la Mecánica D quedó validada sobre acceso *sintético* (η controlado). El único modo de cerrar ese caveat es acceso REAL del agente — que es exactamente el Event Log del SentryLog (PRD §3, §4.1, "el 80% del valor"). Este spec construye la fundación durable + el primer payoff de convergencia: medir si el acceso real de un agente tiene la estructura secuencial (η>0) que D explota.

## Meta

Una fundación de log append-only para Matrix Sentry — journal segmentado + keydir en RAM + recuperación tras crash + aislamiento por tenant + un evento `Access` tipado — y un **analizador** que mide η/predictibilidad de la secuencia de acceso real reusando el mismo `refine.Markov` de la Mecánica D. Producto y validación de I+D en una pieza.

## Decisiones de diseño (cerradas)

- **Payload = JSON (stdlib)**, framing = el del PRD (todo stdlib: `encoding/binary`, `hash/crc32`). Cero deps. JSON legible → depurable a ojo (`tail` al journal). Cambiable a CBOR luego sin tocar el framing.
- **Producto separado de la I+D:** paquetes nuevos bajo `sentry/`; `pq/ivf/internal` intactos.
- **Single-writer (mutex):** serializa appends → `seq` monótono y orden temporal sin locks de transacción.
- **Wall-clock real** en `tstamp` (el determinismo era propiedad del motor de I+D, no del journal).

## Arquitectura

```
sentry/record.go     framing + (de)serialización del header; crc; tipos Record/EventType/TenantID/Seq
sentry/journal.go    segmentos append-only (NNNNNN.log), rotación por tamaño, append/scan/recover
sentry/store.go      API pública: Open/Append/Read/Scan/Close; keydir; filtro por tenant
sentry/access/       Analyze(): Access events -> secuencia de items -> refine.Markov -> Report(lift, entropías)
cmd/sentrydemo/      (opcional) demo: append eventos, recover, Analyze; imprime el Report
```
`sentry/access` importa `matrixsentry/internal/refine` (reusa `Markov`, ya testeada). `sentry` no importa `pq/ivf`.

## Formato de registro (en disco)

```
[ crc32(4) | seq(8) | tstamp(8) | type(1) | tenant(2) | len(4) | payload(len bytes, JSON) ]
```
- `crc32` (IEEE) sobre los bytes `seq..payload` (todo menos el propio crc). Detecta torn writes en recovery.
- `seq` uint64 LE monótono desde 1 · `tstamp` int64 LE unix-nanos · `type` uint8 · `tenant` uint16 LE · `len` uint32 LE = bytes del payload JSON.
- Header fijo = 4+8+8+1+2+4 = 27 bytes, luego `len` bytes de payload.

## Estructura de datos

```go
type Seq uint64
type TenantID uint16
type EventType uint8

const (
    EventAccess EventType = 1 // una memoria/decisión fue accedida (recall/lookup)
    // (EventDecision, EventChange, ... en specs posteriores)
)

type Record struct {
    Seq     Seq
    Tstamp  int64
    Type    EventType
    Tenant  TenantID
    Payload []byte // JSON
}

type location struct{ seg uint32; off int64; size uint32 }

type Store struct {
    dir     string
    opt     Options
    mu      sync.Mutex
    cur     *os.File   // segmento activo
    curSeg  uint32
    curOff  int64
    nextSeq Seq
    keydir  []location // indexado por (seq-1)
    // ... última sincronización, etc.
}

type Options struct {
    SegmentMax  int64         // bytes por segmento antes de rotar (default 256<<20)
    FsyncEvery  time.Duration // group-commit (default 75ms); 0 => fsync en cada Append (tests)
}

// payload del evento Access (mínimo para medir estructura ya; ampliable en etapa 2 vectorial)
type AccessPayload struct {
    ItemID uint64 `json:"item"`
}
```

## API pública

```go
func Open(dir string, opt Options) (*Store, error)                       // crea dir si falta; recupera
func (s *Store) Append(tenant TenantID, t EventType, payload any) (Seq, error) // JSON-marshal, frame, append, fsync por política
func (s *Store) Read(seq Seq) (Record, error)                            // O(1) vía keydir
func (s *Store) Scan(f Filter, fn func(Record) bool) error               // orden de seq; filtra; fn devuelve false para parar
func (s *Store) Close() error                                            // flush + fsync + cierra

type Filter struct {
    Tenant *TenantID  // nil = todos (uso administrativo); en la práctica siempre se pasa un tenant
    Type   *EventType // nil = todos
}
```

## Flujo de datos

**Append:** `mu.Lock`; marshal payload→JSON; construir header con `nextSeq`; escribir header+payload al segmento activo; registrar `keydir[seq-1]`; `nextSeq++`; rotar si `curOff ≥ SegmentMax`; fsync según política; `mu.Unlock`; devolver seq.

**Read(seq):** `loc := keydir[seq-1]`; abrir/seek el segmento; leer header+payload; verificar crc; devolver Record.

**Scan(filter, fn):** recorrer segmentos en orden de seq; por registro, verificar crc, aplicar filtro (tenant/type), llamar `fn`; parar si `fn` devuelve false.

**Recovery (en Open):** listar `*.log` ordenados; por cada uno, leer registros secuencialmente verificando crc; construir keydir y `nextSeq`. Si un registro tiene crc inválido o está truncado **y es el último del último segmento** → truncar el archivo a su offset y terminar (torn tail). Corrupción en cualquier otra posición → error (`ErrCorrupt`).

**Analyze (sentry/access):** `Scan` los `EventAccess` de un tenant en orden → `[]uint64` de ItemIDs. Una **única pasada causal online** (espeja cómo predice D: predecir *antes* de observar, sin ver el futuro):
- Mantener `refine.Markov` y un contador marginal, ambos construidos solo con los pasos `<t`.
- En cada paso `t>0`: `markovHit_t = (Markov.Predict(item[t-1],1) == item[t])`; `marginalHit_t = (argmax del contador marginal hasta t-1 == item[t])`.
- `lift = mean(markovHit) − mean(marginal_hit)` sobre los pasos con estado previo visto.
- Tras evaluar el paso, `Observe(item[t-1], item[t])` e incrementar el marginal (aprender para el futuro).
- Reportar además `Hnext`, `HnextGivenPrev` (entropías estimadas de los conteos finales), `nTransitions`, `coverage` (fracción de pasos con `item[t-1]` ya visto).
- Interpretación: `lift>0` ⇒ la premisa de la Mecánica D se sostiene en acceso real; `lift≈0` ⇒ D inútil en práctica (negativo honesto).

## Durabilidad / consistencia

Group-commit: un flusher fsync-ea cada `FsyncEvery`; `SyncAlways` (FsyncEvery=0) fuerza fsync por append (para tests de crash deterministas). Un solo escritor garantiza que un registro se escribe entero antes del siguiente; el crc detecta el caso de proceso muerto a media escritura del último registro.

## Aislamiento por tenant (criterio de aceptación PRD §11)

`tenant` en el header; `Scan`/`Read`+filtro nunca cruzan tenants. Test explícito: escribir intercalado tenants A y B; `Scan{Tenant:A}` devuelve solo A.

## No-goals (YAGNI)

Sin object-store/CAS (BLAKE3), sin MCP, sin dedup index (task.check), sin compactación/GC de segmentos, sin la etapa 2 vectorial (recall sobre embeddings). Cada uno = su propio spec.

## Plan de pruebas (TDD)

1. **record:** encode→decode del header round-trip; crc detecta un byte alterado.
2. **Append/Read:** round-trip de seq/tstamp/type/tenant/payload; seq monótono.
3. **Scan filtra:** por type; por tenant (**aislamiento**: A no ve B).
4. **Rotación:** SegmentMax pequeño → múltiples segmentos; Read/Scan cruzan segmentos.
5. **Recovery torn-tail:** escribir N, truncar el último registro a la mitad / corromper su crc, reabrir → recupera N−1, `nextSeq=N`, y un Append nuevo continúa bien.
6. **Recovery corrupción media:** crc malo en un registro no-final → `ErrCorrupt`.
7. **access.Analyze:** stream sintético con estructura (cadena) → `lift>0`; stream i.i.d. → `lift≈0` (espejo de la sanidad η=0 de la Mecánica D).

## Criterio de éxito

API `Open/Append/Read/Scan/Close` con los 7 tests verdes (incl. crash-recovery y aislamiento por tenant). `access.Analyze` reportando `lift` sobre una secuencia. Demostrar end-to-end (cmd/sentrydemo o test): appendear una secuencia de Access con estructura, matar+recuperar, y que `Analyze` reporte `lift>0` — la fundación durable Y la primera medición de estructura de acceso real lista para alimentar la Mecánica D con datos del producto.
