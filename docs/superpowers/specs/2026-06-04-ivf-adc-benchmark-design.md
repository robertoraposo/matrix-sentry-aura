# IVFADC Benchmark (Fase 2a) — Design Spec

**Fecha:** 2026-06-04
**Estado:** Aprobado, en implementación
**Contexto:** El motor PQ reproduce el baseline FAISS sobre SIFT1M (recall1@100=0.92 con M=8,
1.000 con M=32). El recall está resuelto; el enemigo es la latencia del scan lineal
(1.86–6.35 ms/query al escanear los 1M códigos). IVFADC parte la linealidad: particiona
en celdas y escanea solo `nprobe` de ellas.

## Meta

Fase 2a: un benchmark (`cmd/ivf1m`) que demuestre que IVFADC residual baja la latencia
~10–50× respecto al scan PQ completo, conservando (o superando) el recall. Si los números
salen, Fase 2b refactoriza a un paquete `ivf/` de producción (Save/Load, API). Cada fase
es su propio spec.

## Decisiones de diseño

- **Residual (IVFADC, FAISS-style)**, no flat. Se encodean residuales `v − centroide(celda)`;
  como tienen menor varianza, el PQ los cuantiza más fino → recall puede superar al scan completo.
- **`pq` queda CONGELADO.** IVFADC reusa `pq.New/Train/Encode` pasándoles residuales, y lee
  los campos exportados `q.Codebooks/M/K/Dsub` para reconstruir la tabla ADC y el loop de
  scoring por celda (no se puede reusar `pq.Search`, que asume una sola tabla sobre todos los códigos).
- **Coarse quantizer = k-means propio sobre D-completo** (~40 líneas, determinista, seeded).
  No puede ser un PQ porque PQ exige K≤256 y nlist=1024. Duplicación consciente del `kmeans`
  privado; en Fase 2b se decide compartir/exportar.

## Arquitectura

```
cmd/ivf1m/main.go   NUEVO  benchmark, barre nprobe (espejo de cmd/sift1m)
ivf/ivf.go          NUEVO  IVFADC: build (Train/Add) + Search
ivf/coarse.go       NUEVO  k-means sobre D-completo, determinista
pq/                 CONGELADO  reusa pq.New/Train/Encode + lee q.Codebooks
```
`ivf` importa `pq`; `pq` no importa `ivf`; sin ciclos.

## Estructura de datos

```go
type IVFADC struct {
    coarse [][]float32   // nlist centroides, dim D
    pq     *pq.PQ        // entrenado sobre residuales
    lists  [][]int32     // lists[c] = IDs base en la celda c
    codes  [][][]uint8   // codes[c][j] = código PQ del residual del j-ésimo vec de la celda c
}
```

## Flujo de datos

**Build (una vez):**
1. `coarse = kmeans(muestra_base, nlist, iter, seed)` — determinista.
2. Asignar cada vector base a su celda más cercana → `lists` (argmin, empate→menor índice).
3. `residual_i = base_i − coarse[celda_i]`; `pq.Train(residuales_del_learn)`.
4. `codes[c][j] = pq.Encode(residual)`.

**Search (por query, con nprobe):**
1. Distancia query→nlist centroides; tomar las `nprobe` celdas más cercanas.
2. Por celda probada `w`: `r = query − coarse[w]`; tabla ADC desde `r`; puntuar `codes[w]`; heap topK.
3. Devolver topK fusionado.

## Determinismo (propiedad a preservar)

- Coarse k-means seed fijo → centroides idénticos a cualquier nº de cores.
- Asignación a celdas: argmin determinista (empate → menor índice).
- Orden de listas: orden de inserción (índice base) — estable.
- Búsqueda paralela sobre queries (read-only) → mismo resultado GOMAXPROCS=1 vs 8.
  **Se verifica explícitamente con un test.**

## Parámetros (defaults, ajustables por flag)

`-nlist 1024` · `-nprobe` barrido `{1,8,16,32,64,128}` · `-m 8 -k 256 -iter 25` · seed fijo.
Regla FAISS: nlist≈√N=1000 para 1M.

## Criterio de éxito

Tabla por nprobe: **recall1@{1,10,100}, inter@100, µs/query, % índice escaneado**, más una
línea baseline = scan PQ completo. Éxito = a nprobe 16–32, recall ≈ scan completo y latencia
~10–50× menor.

## Plan de pruebas

Go local (Mac, go1.25) para desarrollo y unit tests; cross-compile → VM x86_64 para el run
SIFT1M real. Tests:
1. Build/Search sobre datos sintéticos pequeños (corrección).
2. Determinismo: mismo resultado GOMAXPROCS=1 vs 8.
3. Monotonía: recall crece (o iguala) al subir nprobe; nprobe=nlist ≈ scan completo.
4. Sanidad residual: recall IVFADC ≥ recall scan PQ plano a nprobe alto.

## Fase 2b (follow-on, spec aparte)

Refactor a paquete `ivf/` con `New/Train/Add/Search/Save/Load`, decidir compartir el k-means
con `pq`, API de producción para Matrix Sentry.
