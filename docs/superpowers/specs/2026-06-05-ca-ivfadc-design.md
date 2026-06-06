# Content-Addressed IVFADC (CA-IVFADC) — Design Spec

**Fecha:** 2026-06-05
**Estado:** Aprobado, pendiente de plan de implementación
**Fase:** 2b (producción), sobre Fase 2a (benchmark IVFADC, validado) y el build-time fix (2026-06-05, 4.4×).

## Contexto

El motor IVFADC residual reproduce/supera el baseline FAISS sobre SIFT1M en CPU-only
(nprobe=16: recall1@100≈0.92, 5× menos latencia que el scan PQ plano, 1.8% del índice
escaneado). Construir cuesta ~2m11s (train) + ~53s (add 1M). Fase 2b lo lleva a API de
producción para Matrix Sentry (**memoria de decisiones/cambios del agente**; frontera: no es
el store de runtime, eso es MokoBlinks — no mezclar).

En I+D elegimos NO copiar el modelo FAISS de "ID externo opaco". En su lugar, la identidad de
cada vector es **intrínseca a su geometría cuantizada** — algo que el índice ya computa y hoy
descarta.

## La contribución novel: la escalera de identidad

Para un vector `v` asignado a la celda `c`, con residual `r = v − coarse[c]` y código PQ
`code = Encode(r)`, la identidad NO es un contador asignado. Es una **consulta a la resolución
elegida**, en tres niveles, todos derivados de cómputo que ya hacemos:

| Nivel | Clave | Semántica | Coste |
|---|---|---|---|
| Exacto | `FNV-1a(bytes float32 de v)` | "el mismísimo vector, bit a bit" | gratis |
| Cuantizado | `(cell, code)` | "el índice no los distingue" (huella con pérdida) | gratis |
| Tolerante (τ) | `code` dentro de Hamming τ de otro | "decisión esencialmente igual"; τ: exacto→semántico | scan de celda, opt-in |

**Por qué es sólido (no un truco):**
- El hash exacto (Nivel 1) es la **clave primaria**: nunca conflata vectores distintos. Ancla de corrección.
- `(cell, code)` (Nivel 2) es una huella *con pérdida* → NO única; es un bucket de similitud, jamás clave primaria.
- Hamming sobre el código PQ (Nivel 3) es un proxy real y barato de distancia de residual; τ es una
  perilla principiada de "cuán parecido cuenta como lo mismo".

**Honestidad operativa:** los embeddings flotantes rara vez repiten bytes, así que el hash exacto
solo caza repeticiones literales (fast-path/ancla). El feature útil para memoria de agente es el
**tolerante**, expuesto como query explícita (`Recall`) donde su coste se reconoce — NO se ejecuta
en cada `Add` a escondidas.

## API pública

```go
type Config struct {
    Dim, Nlist, M, K, Iter int
    Seed                   int64
    Train                  TrainOpts // CoarseIter, CoarseSample, PQSample (build-time fix)
}

func New(cfg Config) (*Index, error)             // valida cfg; no entrena todavía
func (ix *Index) Train(learn [][]float32) error  // aprende coarse + PQ residual

func (ix *Index) Add(vecs [][]float32) []AddResult

type Handle struct {
    Hash uint64  // FNV-1a del vector crudo — identidad exacta y clave externa estable
    Cell uint32  // celda coarse
    Code []uint8 // código PQ de M bytes — huella cuantizada
}
type Status int // Inserted | ExactDuplicate
type AddResult struct {
    Handle Handle
    Status Status
    Prior  uint64 // si ExactDuplicate, el Hash del handle previo que coincidió
}

type Hit struct { Handle Handle; Dist float32 }
func (ix *Index) Search(q []float32, nprobe, topK int) []Hit
func (ix *Index) SearchBatch(qs [][]float32, nprobe, topK int) [][]Hit

// Recall responde "¿ya conozco algo como esto?" a la resolución tol (Hamming sobre el código
// PQ en la celda del vector). tol=0 → mismo código exacto; tol≥1 → vecindario semántico.
func (ix *Index) Recall(v []float32, tol int) (Handle, bool)

func (ix *Index) Save(w io.Writer) error
func Load(r io.Reader) (*Index, error)

func (ix *Index) Ntotal() int
```

### Semántica
- **`New`/`Train`** separa validación de configuración del entrenamiento (estilo `pq.New`+`Train`),
  reemplazando los 6 args posicionales de la `Train` de prototipo. `Train` internamente usa
  `TrainWithOpts` (build-time fix) con `cfg.Train`.
- **`Add`** hace dedup exacto barato vía dos maps: `hash→pos` y `(cell,code)→pos`. La resolución
  de duplicados ocurre en la fase serial de append (orden de entrada) → determinista. Status:
  `Inserted` o `ExactDuplicate(Prior)`. No inserta el duplicado exacto; devuelve el handle previo.
- **`Search`/`SearchBatch`** devuelven `Hit{Handle, Dist}`. El caller mapea `Handle.Hash`→su
  registro de decisión. El índice no guarda payloads (frontera limpia).
- **`Recall(v, tol)`** es la perilla novel y la única operación cara; opt-in. Encodea `v`, halla su
  celda, escanea los códigos de esa celda dentro de Hamming `tol`, devuelve el handle previo más
  cercano (por distancia ADC dentro del radio) si existe.
- **`Save`/`Load`** con `gob` (como `pq`).

## Estructura de datos

```go
type Index struct {
    cfg    Config
    coarse [][]float32     // nlist centroides, dim D
    pq     *pq.PQ          // entrenado sobre residuales (pq CONGELADO)
    lists  [][]int32       // lists[c] = posiciones (orden de inserción) en la celda c
    codes  [][][]uint8     // codes[c][j] = código PQ del residual del j-ésimo vec de la celda c
    hashes []uint64        // hashes[pos] = FNV-1a del vector crudo (clave exacta por posición)
    cells  []uint32        // cells[pos] = celda del vector pos (para reconstruir Handle)
    ntotal int

    byHash map[uint64]int32       // hash→pos (dedup exacto; derivado, NO serializado)
    byCode map[codeKey]int32      // (cell,code)→pos (huella cuantizada; derivado, NO serializado)
}
```

`codeKey` = string(append(celda, code...)) o equivalente hashable. `byHash`/`byCode` se
reconstruyen en `Load` recorriendo `hashes`/`cells`/`codes`.

## Flujo de datos

**Build:** `New(cfg)` valida → `Train(learn)` = `coarseKMeans` + residuales + `pq.Train`
(vía `TrainWithOpts`, con subsample FAISS).

**Add(vecs):**
1. Fan-out paralelo: por cada vec → celda `c = nearest(coarse, v)`, `code = pq.Encode(v−coarse[c])`,
   `h = FNV-1a(v)`. (read-only sobre el índice → cross-core seguro).
2. Fase serial en orden de entrada: si `h ∈ byHash` → `ExactDuplicate(Prior=hashes[byHash[h]])`,
   no insertar. Si no → asignar `pos = ntotal++`, append a `lists[c]`/`codes[c]`, set
   `hashes[pos]/cells[pos]`, registrar en `byHash`/`byCode`, status `Inserted`.

**Search(q,nprobe,topK):** como Fase 2a (probe nprobe celdas, tabla ADC por celda, heap topK),
pero cada resultado se envuelve en `Handle{hashes[pos], cells[pos], codes[...][...]}`.

**Recall(v,tol):** `c = nearest(coarse,v)`; `code = pq.Encode(v−coarse[c])`; escanear `codes[c]`,
quedarse con los de Hamming(code, ·) ≤ tol; devolver el de menor distancia ADC, o `false`.

## Determinismo (propiedad a preservar)

- Hash y código derivan de geometría sembrada → deterministas.
- `Add`: encode paralelo, pero **append + dedup en fase serial en orden de entrada** → handles y
  status byte-idénticos en 1 vs N cores.
- `Search`: read-only, paralelo sobre queries → idéntico a serial.
- Desempate: argmin de celda → menor índice; heap → menor pos (como Fase 2a).

## Persistencia

`Save` serializa un `snapshot` de campos exportados (`Cfg, Coarse, PQ, Lists, Codes, Hashes,
Cells, Ntotal`) con `gob`. `pq.PQ` ya es gob-serializable. `byHash`/`byCode` NO se serializan; se
reconstruyen en `Load`. Tamaño extra por la identidad: `hashes` 8 B/vec + `cells` 4 B/vec ≈ 12 MB
en 1M — trivial frente a los códigos (8 MB) y centroides.

## No-goals (YAGNI)

- **k-means NO se comparte/exporta con `pq`.** `pq` queda congelado; la coarse vive en `ivf`.
- Sin metadata/payload arbitrario embebido (rompe la frontera Matrix Sentry vs store).
- Near-duplicate NO se autocorre en cada `Add` (coste); vive solo en `Recall`.
- `Recall` devuelve el handle más cercano (no top-N) — ampliable después si hace falta.

## Arquitectura de archivos

```
ivf/index.go     NUEVO  API pública: Index, Config, Handle, AddResult, Hit, New/Train/Add/Search/Recall
ivf/identity.go  NUEVO  FNV-1a sobre float32, Hamming sobre código PQ, la escalera de identidad
ivf/persist.go   NUEVO  snapshot + Save/Load (gob), reconstrucción de byHash/byCode
ivf/ivf.go       EXISTE  motor interno (coarse assign, ADC table, scoring) — refactor a privado bajo Index
ivf/coarse.go    EXISTE  sqL2, nearest
ivf/kmeans.go    EXISTE  coarseKMeans, subsample, TrainOpts
ivf/heap.go      EXISTE  maxHeap
pq/              CONGELADO
cmd/ivf1m/       EXISTE  benchmark (sigue usando el motor; opcional: demo de Recall/dedup)
```

`ivf` importa `pq`; sin ciclos.

## Plan de pruebas (TDD)

1. **Identidad exacta:** añadir el mismo vector dos veces → 2º `AddResult` = `ExactDuplicate`,
   `Prior` = hash del 1º; `Ntotal` no crece.
2. **Escalera tolerante:** dos vectores con códigos a Hamming 1 → `Recall(v, 0)` no los une,
   `Recall(v, 1)` sí.
3. **Determinismo cross-core:** mismo corpus en GOMAXPROCS 1 vs 8 → mismos handles y mismos status.
4. **Round-trip Save/Load:** `Search` y `Recall` dan resultados idénticos antes y después de
   `Save`→`Load` (incl. dedup: un `Add` de duplicado tras `Load` se detecta).
5. **Validación de Config:** `New` rechaza Dim%M≠0, K>256, Nlist≤0, etc.
6. **Equivalencia con Fase 2a:** sobre el mismo seed/params, `Search` da el mismo recall que el
   `IVFADC` de prototipo (la identidad no degrada el ranking).

## Criterio de éxito

- API `New/Train/Add/Search/Recall/Save/Load` con los 6 tests verdes, determinismo incluido.
- Round-trip Save/Load sobre el índice SIFT1M real (VM): construir una vez, persistir, cargar,
  y reproducir el recall de Fase 2a — demostrando "build once, serve instantly".
- Demo de la escalera: añadir un embedding, volver a añadir uno idéntico → reconocido; `Recall`
  de un casi-duplicado a τ creciente → lo encuentra.
