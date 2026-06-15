/* MATRIX SENTRY — synthetic semantic-memory corpus generator.
   Exposes window.MatrixCorpus. Deterministic per-tenant via seeded PRNG. */
(function () {
  "use strict";

  function mulberry32(a) {
    return function () {
      a |= 0; a = (a + 0x6d2b79f5) | 0;
      let t = Math.imul(a ^ (a >>> 15), 1 | a);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }
  function gauss(rnd) {
    let u = 0, v = 0;
    while (!u) u = rnd();
    while (!v) v = rnd();
    return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
  }
  function pick(rnd, arr) { return arr[Math.floor(rnd() * arr.length)]; }

  // Fibonacci-sphere placement so clusters spread evenly in the volume.
  function clusterCenter(rnd, i, n) {
    const phi = Math.acos(1 - 2 * (i + 0.5) / n);
    const theta = Math.PI * (1 + Math.sqrt(5)) * i + rnd() * 0.6;
    const r = 11 + rnd() * 4.5;
    return [
      r * Math.sin(phi) * Math.cos(theta),
      r * Math.cos(phi) * 0.72,
      r * Math.sin(phi) * Math.sin(theta),
    ];
  }

  const SOURCES = ["slack", "github", "notion", "agent", "email", "ticket", "pr", "log"];

  // Palette of cluster hues — cyan / magenta / amber forward, with support hues.
  const HUES = {
    cyan: "#35E6FF", magenta: "#FF3DCB", amber: "#FFB23E",
    green: "#34E5A0", violet: "#9B6CFF", blue: "#4D8DFF",
    rose: "#FF6B8B", lime: "#9DEE4E",
  };

  // ---- Theme content pools (Spanish, realistic one-liners) ----
  const THEMES = {
    // BlazeSphere — devops / infra
    deploy: { label: "deploy", color: HUES.cyan, tags: ["deploy", "release", "ci"], pool: [
      "Rollback de v2.3.1 por timeout en la migración de índices",
      "Canary al 5% en us-east-1, métricas estables 20 min",
      "Blue/green promovido a producción tras smoke tests verdes",
      "Pipeline de CI falla en step de cache — limpiar layer de docker",
      "Hotfix v2.3.2: corrige fuga de conexiones en el pool de PG",
      "Deploy congelado durante el viernes — política de freeze activa",
      "Feature flag 'galaxy-v2' al 100% sin regresiones de latencia",
      "Artefacto firmado con cosign antes de subir al registry",
    ]},
    auth: { label: "auth", color: HUES.magenta, tags: ["auth", "oauth", "seguridad"], pool: [
      "OAuth: el claim de tenant debe ir en el access_token, no en el id_token",
      "Bearer rotado cada 24h; refresh con grant de client_credentials",
      "Bug: el scope 'memory:write' no se valida en el endpoint /forget",
      "Aislamiento por llave — cada credencial enruta a un único tenant",
      "MFA obligatorio para llaves con scope admin",
      "Token revocado al detectar uso desde 2 regiones en <60s",
      "Consent screen pide solo los scopes mínimos del agente",
      "Fuga evitada: secreto enmascarado en logs con redacción automática",
    ]},
    infra: { label: "infra", color: HUES.blue, tags: ["infra", "k8s", "red"], pool: [
      "Autoscaler sube de 4 a 11 pods bajo carga de recall",
      "Índice HNSW reconstruido: recall@10 sube de 0.971 a 0.984",
      "Sharding por tenant en el vector store, réplica en lectura",
      "Latencia p99 del recall baja a 0.7ms tras warm-up del caché",
      "Backup append-only del journal cada 5 min a almacenamiento frío",
      "Node pool spot interrumpido — drenado limpio sin pérdida de datos",
      "Embeddings de 1024-d cuantizados a int8 para ahorrar memoria",
      "Health-check del gateway de credenciales en verde",
    ]},
    decisiones: { label: "decisiones", color: HUES.amber, tags: ["decisión", "rfc", "arquitectura"], pool: [
      "RFC-014: dedup por radio τ=0.12 en espacio coseno aprobado",
      "Decidimos journal append-only, nunca borrado físico de memorias",
      "Elegimos 768-d para tenants pequeños, 1024-d para corpus grandes",
      "Promote-to-memory requiere doble confirmación del agente dueño",
      "Comms entre agentes pasa por canal por área, no broadcast global",
      "El recall devuelve k=10 vecinos por defecto, configurable por llave",
      "Acordado: sin PII en memorias; tokenizar antes de vectorizar",
      "Política de retención: memorias frías se archivan a los 90 días",
    ]},
    gotchas: { label: "gotchas", color: HUES.violet, tags: ["gotcha", "bug", "nota"], pool: [
      "Ojo: el coseno no normaliza si el vector llega con NaN del modelo",
      "Trampa: dos tenants con el mismo texto NO deben compartir vector",
      "El timestamp del journal debe ser monótono, no usar wall-clock",
      "Cuidado: InstancedMesh no actualiza si no marcas needsUpdate",
      "Gotcha: el bearer caduca a medianoche UTC, no a las 24h exactas",
      "El recall en frío tarda 8ms — precalentar el índice al arrancar",
      "Bug raro: el halo del clúster parpadea con bloom > 1.4",
      "Nota: forget marca tombstone, el GC lo limpia en background",
    ]},
    // Kuadre — fintech
    pagos: { label: "pagos", color: HUES.green, tags: ["pagos", "checkout", "psp"], pool: [
      "Conciliación de pagos con el PSP corre a las 03:00 AST",
      "Retry idempotente en cargos con clave de idempotencia por orden",
      "Soporta tarjeta, transferencia y billetera en el checkout único",
      "3DS challenge sube conversión 4% en montos > RD$5,000",
      "Webhook de pago confirmado dispara la captura del fondo",
      "Split de pago a 3 comercios validado en sandbox",
      "Reembolso parcial deja traza en el journal de transacciones",
      "Timeout del adquirente: reintentar máx 3 veces con backoff",
    ]},
    fraude: { label: "fraude", color: HUES.rose, tags: ["fraude", "riesgo", "ml"], pool: [
      "Score de riesgo > 0.8 manda la orden a revisión manual",
      "Patrón de tarjetas probadas: 40 intentos en 2 min desde 1 IP",
      "Lista gris por device-fingerprint, no por IP",
      "Falso positivo: cliente recurrente bloqueado por viaje al extranjero",
      "Velocity check: máx 5 cargos por tarjeta por hora",
      "Chargeback ratio del comercio sube a 0.9% — alerta",
      "Modelo de fraude reentrenado con datos del último trimestre",
      "Regla: montos redondos repetidos elevan el score",
    ]},
    onboarding: { label: "onboarding", color: HUES.cyan, tags: ["onboarding", "kyc", "ux"], pool: [
      "KYC con liveness reduce fraude de identidad en 30%",
      "Onboarding de comercio en 3 pasos, abandono baja a 12%",
      "Validación de cédula contra el padrón en < 2s",
      "Pre-llenado de datos desde RNC acelera el alta",
      "Recordatorio por WhatsApp recupera 18% de altas incompletas",
      "Documento borroso: pedir recaptura sin reiniciar el flujo",
      "Verificación de cuenta bancaria con micro-depósito",
      "Checklist de compliance gateado por país",
    ]},
    api: { label: "api", color: HUES.blue, tags: ["api", "sdk", "docs"], pool: [
      "Rate limit por llave: 600 req/min, burst de 100",
      "SDK de JS publica v3 con tipados y reintentos automáticos",
      "Versionado por header, no por path, para no romper clientes",
      "El endpoint /recall acepta texto o vector pre-calculado",
      "Errores con código estable + mensaje localizable",
      "Paginación por cursor opaco, nunca por offset",
      "Webhook con firma HMAC y ventana de tolerancia de 5 min",
      "Sandbox con datos sintéticos espejo de producción",
    ]},
    soporte: { label: "soporte", color: HUES.amber, tags: ["soporte", "tickets", "sla"], pool: [
      "Macro de respuesta para 'no llegó mi transferencia'",
      "SLA de primera respuesta < 15 min en horario hábil",
      "Agente IA resuelve 62% de tickets de nivel 1 solo",
      "Escalamiento a humano cuando el sentimiento cae 2 turnos",
      "Base de conocimiento sincronizada con el corpus de memoria",
      "Etiqueta 'recurrente' agrupa al cliente con su historial",
      "Handoff conserva el contexto completo de la conversación",
      "Encuesta CSAT tras cierre, promedio 4.6/5",
    ]},
    // Round PlayGames — gaming
    matchmaking: { label: "matchmaking", color: HUES.cyan, tags: ["matchmaking", "elo", "latencia"], pool: [
      "MMR basado en Glicko-2, ventana de espera adaptativa",
      "Pareo por región para mantener ping < 60ms",
      "Backfill de partidas cuando un jugador abandona el lobby",
      "Cola de ranked separa parties de jugadores solo",
      "Penalización por dodge sube exponencial tras 3 abandonos",
      "Predicción de skill con incertidumbre alta en cuentas nuevas",
      "Tiempo de cola promedio en oro: 48s",
      "Smurf detection por curva de winrate anómala",
    ]},
    economia: { label: "economía", color: HUES.green, tags: ["economía", "store", "tuning"], pool: [
      "Drop rate del cofre legendario ajustado a 0.7%",
      "Sink de moneda con skins de temporada estabiliza inflación",
      "Pase de batalla retiene 22% más en la semana 3",
      "Precio dinámico de gemas por región y poder adquisitivo",
      "Evento de doble XP dispara ventas de boosters",
      "Mercado de items con comisión del 8% al vendedor",
      "Refund window de 2h para compras accidentales",
      "Curva de progresión re-balanceada en niveles 30-45",
    ]},
    anticheat: { label: "anticheat", color: HUES.rose, tags: ["anticheat", "ban", "señal"], pool: [
      "Aimbot detectado por entropía anómala del input del mouse",
      "Ban wave semanal con apelación en 72h",
      "Señal de wallhack: visión de enemigos tras pared sin línea",
      "Kernel anticheat opcional para ranked alto",
      "Shadow-ban agrupa tramposos en sus propias partidas",
      "Falso positivo por macro de accesibilidad — whitelist",
      "Telemetría de servidor, nunca confiar en el cliente",
      "Detección de boosting por patrón de cuentas coordinadas",
    ]},
    eventos: { label: "eventos", color: HUES.amber, tags: ["eventos", "live-ops", "temporada"], pool: [
      "Evento de invierno: mapa nevado y modo 4v4 limitado",
      "Live-ops empuja misión diaria sin redeploy del cliente",
      "Colaboración con marca duplica jugadores activos el finde",
      "Temporada 7 arranca con reset de rangos suave",
      "Recompensa de login escalonada en 7 días",
      "Torneo comunitario con bracket auto-generado",
      "Teaser del próximo héroe filtra hype en redes",
      "Rollback de evento por exploit de farmeo de moneda",
    ]},
    social: { label: "social", color: HUES.violet, tags: ["social", "clanes", "chat"], pool: [
      "Clanes con cofre compartido y misiones cooperativas",
      "Chat de voz por proximidad en el modo battle royale",
      "Sistema de amigos sugiere por jugadas recientes juntas",
      "Reporte de toxicidad con revisión por modelo de lenguaje",
      "Emotes de temporada como vector de identidad del jugador",
      "Fiestas de hasta 5, cross-play entre consola y PC",
      "Tabla de líderes por clan y por región",
      "Mute inteligente silencia spam pero no coordinación",
    ]},
    // Personal
    ideas: { label: "ideas", color: HUES.cyan, tags: ["idea", "side-project"], pool: [
      "App que convierte notas de voz en tareas con fecha",
      "Visualizar mi historial de lectura como una galaxia",
      "Bot que resume newsletters y me deja solo lo accionable",
      "Jardín digital de notas enlazadas tipo zettelkasten",
      "Medidor de foco que cruza calendario y tiempo de pantalla",
      "Generador de itinerarios a partir de un mapa de antojos",
      "Plugin que detecta promesas en mis mensajes y las agenda",
      "Diario que pregunta una cosa distinta cada noche",
    ]},
    recetas: { label: "recetas", color: HUES.amber, tags: ["receta", "cocina"], pool: [
      "Mangú con los tres golpes, cebolla bien encurtida",
      "Masa de pizza fermentada 48h en frío sabe muchísimo mejor",
      "Sancocho de siete carnes para domingo de lluvia",
      "Café cortado: 1 parte espresso, 1 leche texturizada",
      "Arroz con coco y habichuelas negras del sur",
      "Pollo al horno con limón, ajo y orégano fresco",
      "Tres leches con un toque de ron añejo",
      "Guacamole sin tomate, más lima de lo que crees",
    ]},
    viajes: { label: "viajes", color: HUES.green, tags: ["viaje", "lugares"], pool: [
      "Amanecer en las Dunas de Baní, llegar antes de las 6",
      "Ruta del café en Jarabacoa, llevar capa por la neblina",
      "Los Haitises en kayak con marea alta",
      "Cabarete para kite, viento fuerte de enero a marzo",
      "Constanza huele a pino, lleva abrigo aunque sea agosto",
      "Bahía de las Águilas: ir temprano, no hay sombra",
      "Casco colonial a pie al atardecer, calle Las Damas",
      "27 Charcos de Damajagua: zapatos de agua sí o sí",
    ]},
    libros: { label: "libros", color: HUES.violet, tags: ["libro", "lectura"], pool: [
      "Releer el capítulo de memoria episódica vs semántica",
      "Ensayo sobre sistemas que envejecen bien con el uso",
      "Novela donde la ciudad recuerda a quien la habita",
      "Cita: 'lo que no se nombra, no se puede recordar'",
      "Manual de diseño de interfaces calmadas",
      "Biografía de una matemática que ordenó el caos",
      "Cuento corto sobre un archivo que sueña",
      "Tratado breve sobre el arte de tomar notas",
    ]},
    salud: { label: "salud", color: HUES.rose, tags: ["salud", "hábitos"], pool: [
      "Caminar 8k pasos baja mi frecuencia en reposo",
      "Dormir antes de medianoche cambia todo el día siguiente",
      "Hidratarse primero, el café después del primer vaso",
      "Movilidad de cadera 5 min antes de sentarme a trabajar",
      "Pausa de pantalla cada 50 min, mirar lejos 20s",
      "Respiración 4-7-8 para bajar antes de dormir",
      "Fuerza 2x semana mantiene la espalda sin quejas",
      "Sol de la mañana ancla el reloj circadiano",
    ]},
  };

  const TENANTS = [
    { key: "blazesphere", name: "BlazeSphere", glyph: "◆", accent: "#35E6FF",
      themes: ["deploy", "auth", "infra", "decisiones", "gotchas"],
      agents: ["agent-orion", "agent-vega", "agent-atlas", "agent-echo"] },
    { key: "kuadre", name: "Kuadre", glyph: "▲", accent: "#FF3DCB",
      themes: ["pagos", "fraude", "onboarding", "api", "soporte"],
      agents: ["agent-lyra", "agent-nova", "agent-iris", "agent-echo"] },
    { key: "roundplay", name: "Round PlayGames", glyph: "●", accent: "#9DEE4E",
      themes: ["matchmaking", "economia", "anticheat", "eventos", "social"],
      agents: ["agent-pixel", "agent-glitch", "agent-arcade", "agent-echo"] },
    { key: "personal", name: "Personal", glyph: "✦", accent: "#FFB23E",
      themes: ["ideas", "recetas", "viajes", "libros", "salud"],
      agents: ["agent-self", "agent-muse"] },
  ];

  let UID = 1;

  function generate(tenantKey, count) {
    const tenant = TENANTS.find((t) => t.key === tenantKey) || TENANTS[0];
    const seed = Array.from(tenant.key).reduce((a, c) => a + c.charCodeAt(0), 7) * 2654435761;
    const rnd = mulberry32(seed);
    const themes = tenant.themes;
    const n = themes.length;
    const centers = themes.map((_, i) => clusterCenter(rnd, i, n));
    const clusters = themes.map((tk, i) => {
      const th = THEMES[tk];
      return { key: tk, label: th.label, color: th.color, center: centers[i], count: 0 };
    });

    const now = Date.now();
    const points = [];
    const per = Math.floor(count / n);
    for (let ci = 0; ci < n; ci++) {
      const th = THEMES[themes[ci]];
      const center = centers[ci];
      const spread = 2.4 + rnd() * 1.6;
      const k = ci === n - 1 ? count - points.length : per;
      for (let j = 0; j < k; j++) {
        const pos = [
          center[0] + gauss(rnd) * spread,
          center[1] + gauss(rnd) * spread * 0.85,
          center[2] + gauss(rnd) * spread,
        ];
        // access frequency: power-law — most cold, a few very hot
        const access = Math.floor(Math.pow(rnd(), 3.2) * 1400) + 1;
        const heat = Math.min(1, access / 420);
        const age = Math.pow(rnd(), 1.4) * 30; // days
        const text = pick(rnd, th.pool);
        const tags = [th.tags[0]];
        if (rnd() > 0.5) tags.push(pick(rnd, th.tags));
        points.push({
          id: "m" + (UID++).toString(36),
          tenant: tenant.key,
          cluster: ci,
          clusterKey: themes[ci],
          clusterLabel: th.label,
          color: th.color,
          pos,
          text,
          tags: Array.from(new Set(tags)),
          source: pick(rnd, SOURCES),
          access,
          heat,
          createdAt: now - age * 86400000,
          dim: rnd() > 0.5 ? 1024 : 768,
        });
        clusters[ci].count++;
      }
    }
    return { tenant, clusters, points };
  }

  // A few seed agent messages per tenant for the Comms kanban.
  function comms(tenantKey) {
    const tenant = TENANTS.find((t) => t.key === tenantKey) || TENANTS[0];
    const seed = Array.from(tenant.key + "comms").reduce((a, c) => a + c.charCodeAt(0), 3) * 40503;
    const rnd = mulberry32(seed);
    const areas = tenant.themes.slice(0, 4).map((tk) => ({ key: tk, label: THEMES[tk].label, color: THEMES[tk].color }));
    const TYPES = [
      { t: "pregunta", color: "#35E6FF" },
      { t: "respuesta", color: "#34E5A0" },
      { t: "info", color: "#9B6CFF" },
    ];
    const prompts = {
      deploy: ["¿Promuevo el canary al 25%?", "Smoke tests verdes, listo para prod.", "Freeze de viernes sigue activo hasta el lunes."],
      auth: ["¿El claim de tenant va en el access_token?", "Confirmado, va en access_token con scope mínimo.", "Rotación de bearer programada a medianoche UTC."],
      infra: ["p99 del recall subió a 1.1ms, ¿warm-up frío?", "Sí, precalienta el índice HNSW al arrancar.", "Autoscaler estable en 9 pods."],
      decisiones: ["¿τ=0.12 para dedup es definitivo?", "Aprobado en RFC-014, no tocar sin nuevo RFC.", "Retención de memorias frías: 90 días."],
      pagos: ["¿Reintento idempotente en timeout del adquirente?", "Sí, máx 3 con backoff y misma clave.", "Conciliación nocturna OK."],
      fraude: ["Pico de tarjetas probadas desde una IP.", "Bloqueado por device-fingerprint, no IP.", "Modelo reentrenado, falsos positivos -12%."],
      onboarding: ["¿Recaptura sin reiniciar el flujo?", "Correcto, no perdemos los pasos previos.", "Liveness baja fraude de identidad 30%."],
      api: ["¿Rate limit por llave o por tenant?", "Por llave: 600/min, burst 100.", "SDK v3 publicado con reintentos."],
      matchmaking: ["Cola de oro en 48s, ¿aceptable?", "Sí, dentro del SLA de pareo.", "Backfill activo al abandonar lobby."],
      economia: ["Drop legendario a 0.7%, ¿muy bajo?", "Es el objetivo, sostiene la economía.", "Pase de batalla retiene +22%."],
      anticheat: ["Posible aimbot por entropía del mouse.", "Confirmado, va a la ban wave del viernes.", "Whitelist para macro de accesibilidad."],
      eventos: ["¿Lanzo el evento de invierno hoy?", "Sí, mapa nevado y 4v4 listos.", "Rollback si reaparece el exploit de farmeo."],
      ideas: ["Idea: galaxia de mi historial de lectura.", "Me encanta, lo agrego al jardín de notas.", "Bot que resume newsletters: priorizar."],
      recetas: ["¿Masa de pizza 48h en frío?", "Sí, sabor muchísimo mejor.", "Mangú con los tres golpes para el domingo."],
      viajes: ["Dunas de Baní antes de las 6am.", "Confirmado, llevamos café y agua.", "Bahía de las Águilas: nada de sombra."],
      libros: ["Releer memoria episódica vs semántica.", "Buenísimo para el diseño del recall.", "Cita: lo que no se nombra no se recuerda."],
      soporte: ["Macro para 'no llegó mi transferencia'.", "Hecho, SLA de primera respuesta <15 min.", "Handoff conserva todo el contexto."],
      social: ["Reporte de toxicidad con modelo de lenguaje.", "Activado, mute inteligente sin tumbar coordinación.", "Cofre de clan compartido listo."],
      gotchas: ["El timestamp del journal debe ser monótono.", "Anotado, nada de wall-clock.", "InstancedMesh necesita needsUpdate."],
    };
    const columns = areas.map((a) => {
      const base = prompts[a.key] || ["Nota del área.", "Confirmado.", "Seguimos."];
      const cards = [];
      const m = 3 + Math.floor(rnd() * 2);
      for (let i = 0; i < m; i++) {
        const ty = TYPES[i % TYPES.length === 0 && i > 0 ? 1 : i % TYPES.length];
        const author = pick(rnd, tenant.agents);
        const target = rnd() > 0.45 ? "@" + pick(rnd, tenant.agents.filter((x) => x !== author)) : null;
        cards.push({
          id: "c" + (UID++).toString(36),
          author,
          type: ty.t,
          typeColor: ty.color,
          target,
          text: base[i % base.length],
          mins: Math.floor(rnd() * 58) + 1,
          reply: i > 0 && rnd() > 0.5,
          promotable: ty.t !== "pregunta" && rnd() > 0.45,
        });
      }
      return { key: a.key, label: a.label, color: a.color, cards };
    });
    return { columns, agents: tenant.agents };
  }

  window.MatrixCorpus = { tenants: TENANTS, generate: generate, comms: comms, themes: THEMES };
})();
