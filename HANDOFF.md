# Matrix Sentry · Handoff a Claude Code

Estado del motor PQ (Product Quantization, Go puro, cero deps, cero Postgres).
Validado en SIFT10K: recall1@1=0.50 / @10=0.87 / @100=0.99, 64x compresion,
~10x mas rapido que exacto, pipeline check 100/100 contra ground truth INRIA.
Train/EncodeBatch/SearchBatch paralelos y deterministas (mismo resultado a
cualquier numero de cores: verificado GOMAXPROCS=1 vs 8).

## Estructura

```
go.mod                 module matrixsentry (go 1.24)
pq/kmeans.go           k-means + k-means++ incremental
pq/pq.go               PQ: New/Train/Encode/EncodeBatch/Search/SearchBatch/Save/Load
cmd/sift1m/main.go     PRUEBA REAL escala-agnostica (SIFT1M o siftsmall via flags)
cmd/sift/main.go       variante fija SIFT10K
cmd/pqdemo/main.go     demo sintetico (ilustrativo, NO prueba)
scripts/get_sift1m.sh  descarga SIFT1M
```

## Arranque en tu homelab (Escenario A)

```bash
go version            # necesita 1.24+
go build ./...        # debe compilar limpio
./scripts/get_sift1m.sh /data/sift
go build -o sift1m ./cmd/sift1m
./sift1m -dir /data/sift -prefix sift -m 8 -k 256 -iter 25
```

Objetivo: caer cerca del baseline FAISS/Jegou (recall1@1~0.22, @10~0.60,
@100~0.92). Si si, el motor reproduce FAISS sobre 1M vectores.

Flags utiles: -m 16 (mas recall, 16B/vector) | -train 50000 (entrenar mas
rapido) | -check 100 (queries a verificar contra ground truth).

## Siguiente: Escenario B (tus embeddings reales)

Prueba sobre TU distribucion, no SIFT. Lo que falta construir (prompt para
Claude Code abajo):

1. Cliente Ollama (endpoint Tesla A2) -> embeddings de tus docs.
   Modelo: nomic-embed-text (768d) con prefijos `search_document:` /
   `search_query:`.  O mxbai-embed-large (1024d).
2. Chunker (512 tokens, overlap 64) sobre corpus tecnico (NO configs de
   clientes; mantener todo en tu Tailscale).
3. Harness: embeber -> PQ.Train/Encode -> ground truth exacto por fuerza bruta
   -> recall1@R e inter@R.  Reusa pq/ tal cual; D=768 -> usar M=96 o M=48.

## Prompt sugerido para Claude Code

> Tengo el paquete matrixsentry (PQ en Go puro, package pq/). Construye
> cmd/embed-eval que: (a) lea .md/.txt de un dir, los chunke a 512 tokens con
> overlap 64, (b) embeba cada chunk via Ollama en {ENDPOINT} con
> nomic-embed-text usando prefijo search_document:, (c) entrene pq.New(768, 96,
> 256) sobre los embeddings, los encode, (d) tome N queries reales (prefijo
> search_query:), calcule ground truth por fuerza bruta (cosine sobre float32) y
> reporte recall1@{1,10,100} e inter@{1,100} del PQ contra ese ground truth.
> No normalices si usas dot; si usas cosine, normaliza antes de PQ y brute force
> de forma consistente. Reusa el package pq sin modificarlo.

## Notas de diseno ya decididas

- MVP Matrix Sentry = Event Log + Diff Store + ADR + dedup exacto (hash, sin
  bloom) + namespacing por tenant (RLS). PQ es la capa semantica, se activa
  cuando float32 plano no alcance (~>1M vectores).
- Frontera: Matrix Sentry = memoria de decisiones/cambios del agente;
  MokoBlinks = logs de runtime. No mezclar.
