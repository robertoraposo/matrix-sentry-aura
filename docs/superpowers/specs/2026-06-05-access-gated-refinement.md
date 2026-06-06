# Access-Gated Residual Refinement — Experiment Spec (R&D)

**Fecha:** 2026-06-05
**Estado:** Aprobado (dirección elegida: "directo al access-gated"), experimento antes de paquete.
**Antecedente:** El diagnóstico `cmd/ivfdiag` ([[ca-ivfadc-error-budget]]) midió: R@100 NO está limitado por cuantización (oracle@100=0.9983); el 4.8pp de gap es de distractores cross-celda recuperables con re-rank; R@1 sí topa (oracle@1=0.52). Water-filling muerto; anisotropía real. El refinement layer cierra el gap y, gated por acceso, es la claim novel defendible.

## Claim a medir (la versión honesta, post prior-art adversarial)

La asignación de bits **per-item por frecuencia de acceso logueada** — `min Σ_i hit_count(i)·D_i(b_i) s.t. Σ b_i ≤ B` ⟹ `b_i* ∝ base + (d/2)·log hit_count(i)`, instanciada como un **refinement residual access-gated** — optimiza el **recall ponderado por acceso por bit** en vez del recall de query uniforme que optimiza FAISS. Prior-art refuta la forma amplia; sobrevive la conjunción de 4 ejes (índice de búsqueda ANN + bits por acceso logueado + residual-refinement como perilla + objetivo recall-per-bit ponderado). Pivote honesto si se colapsa: el **resultado empírico** de que allocation por frecuencia-persistente bate al refine-por-query (DiskANN) en recall-per-bit bajo power-law.

## Mecanismo

- **Base:** IVFADC existente (nlist=1024, M=8, K=256). Cada item: celda `c`, residual `r=x−μ_c`, código base → `r̂`. (8 B/item.)
- **Refine PQ:** un segundo `pq.PQ` entrenado sobre los **errores de primera etapa** `e_i = r_i − r̂_i` (full-D, M2 subespacios, K2=256). Item refinado guarda M2 bytes extra; `ê_i = refinePQ.reconstruct(refineCode_i)`.
- **Gating por acceso:** popularidad `pop(i) = Σ_q w_q·[i ∈ true-top-Rpop(q)]` con `w_q` Zipf sobre las queries. Se refinan los items en el top-φ por popularidad (φ = presupuesto de bytes). Cold items quedan a 8 B.
- **Search + re-rank:** IVFADC base (nprobe) → top-K candidatos por ADC base → re-rank por distancia refinada `‖q − μ_c − r̂_i − ê_i‖²` (usa `ê_i` si el item es hot; si no, `‖q − μ_c − r̂_i‖²`) → top-R. (K = rerankK, directo full-D, sin tabla.)

## Modelo de acceso (sintético — SIFT no tiene log de agente)

- `w_q = 1/(rank_q)^s` tras una permutación determinista (seed) de las 10k queries; `s` = exponente Zipf (default 1.07).
- `pop(i) = Σ_q w_q·[i ∈ gt[q][0:Rpop]]`, Rpop=100 (popularidad se reparte sobre los 100 NN de cada query popular → hot set realista, no un único item por query).
- Hot set = top-`hotfrac` items por `pop` (default 0.10).

## Métrica

Para cada variante: recall1@{1,10,100} **uniforme** y **ponderado por acceso** `Σ_q w_q·hit_q / Σ_q w_q`. Variantes:
- **(a) baseline** — sin refine. 8 B/item.
- **(b) access-gated** — top-φ hot refinados. 8 + M2·φ B/item promedio.
- **(c) full-refine** — todos refinados (cota superior). 8 + M2 B/item.
- **(d) per-query refine (contraste DiskANN)** — re-rank de candidatos usando refine code para TODOS (almacena ê de todos; el contraste es el almacenamiento, no el recall: (d)≈(c) en recall pero paga M2 en todos).

**Headline:** fracción de la ganancia de (c) en recall-ponderado-por-acceso que captura (b), a fracción de los bytes extra. Y la **brecha** uniforme-vs-ponderado de (b): el gated sube el ponderado mucho más que el uniforme — la firma de la tesis.

## Arquitectura

```
internal/lab/     helpers verificados (lectores, geometría, reconstruct, ADC, kmeans) — copia bit-idéntica del engine, compartida por experimentos
internal/refine/  núcleo novel + TESTEADO (TDD): trainRefine, refineReconstruct, refinedDist, zipfWeights, popularity, selectHot, weightedRecall
cmd/ivfrefine/    orquesta: build geometry → refine PQ → access model → search+rerank → tabla (a)(b)(c)(d)
pq/, ivf/         CONGELADOS
```

## Plan de pruebas (TDD sobre internal/refine)

1. `zipfWeights(n,s,seed)`: suma normalizable, decreciente por rank, determinista; s=0 → uniforme.
2. `refinedDist`: `‖q − μ − r̂ − ê‖²` correcta; con ê=0 == distancia base; reduce el error vs base cuando ê aproxima e.
3. `popularity`: items en top-R de queries de alto peso obtienen pop alta; determinista.
4. `selectHot(pop, frac)`: tamaño = round(frac·n), elige los de mayor pop, empate→menor índice.
5. `weightedRecall`: con pesos uniformes == recall uniforme; pondera correctamente.
6. **End-to-end (smoke, datos sintéticos):** refinar los hot sube el recall ponderado más que el uniforme; full-refine ≥ gated ≥ baseline.

## Criterio de éxito

Sobre SIFT1M con Zipf: **(b) access-gated captura la mayoría de la ganancia de (c) en recall-ponderado-por-acceso usando solo φ=10% de los bytes extra**, y la brecha ponderado−uniforme de (b) es claramente positiva. Eso demuestra el resultado empírico defendible. Si (b)≈(a) en ponderado, la premisa falla (oracle ya satura los hot) y se reporta honestamente.
