/* Live-data shim: replaces window.MatrixCorpus.generate/comms with backend data
   when /api/galaxy is reachable, falling back to the original mock otherwise. */
(function () {
  const cache = {};            // tenantKey -> galaxy data
  const commsCache = {};
  const journalCache = {};
  let providersCache = null;
  window.MatrixLive = {
    async prime(tenantKey) {
      try {
        const r = await fetch("/api/galaxy?tenant=" + encodeURIComponent(tenantKey));
        if (!r.ok) throw new Error("galaxy " + r.status);
        cache[tenantKey] = await r.json();
        try {
          const rc = await fetch("/api/comms?tenant=" + encodeURIComponent(tenantKey));
          if (rc.ok) commsCache[tenantKey] = await rc.json();
        } catch (e) { /* comms optional */ }
        try {
          const rj = await fetch("/api/journal?tenant=" + encodeURIComponent(tenantKey) + "&limit=60");
          if (rj.ok) journalCache[tenantKey] = (await rj.json()).events || [];
        } catch (e) { /* journal optional */ }
        try {
          const rp = await fetch("/api/providers");
          if (rp.ok) providersCache = await rp.json();
        } catch (e) { /* providers optional */ }
        if (!this._patched) this._patch();
        this._patched = true;
        console.info("[live] primed", tenantKey, "points:", (cache[tenantKey].points || []).length);
      } catch (e) {
        console.warn("[live] prime failed for", tenantKey, "— using mock:", e.message);
      }
    },
    journalEvents(tenantKey) {
      const e = journalCache[tenantKey];
      return Array.isArray(e) && e.length ? e : null;
    },
    providers() {
      return providersCache || { count: 0, providers: [] };
    },
    async fetchProviders() {
      try {
        const r = await fetch("/api/providers");
        if (!r.ok) return null;
        providersCache = await r.json();
        return providersCache;
      } catch (e) {
        return null;
      }
    },
    async providerAction(action, provider, loginId) {
      const payload = { action, provider };
      if (loginId) payload.loginId = loginId;

      const r = await fetch("/api/providers/action", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      let data = {};
      try { data = await r.json(); } catch (e) {}
      if (!r.ok) throw new Error(data.error || ("provider action " + r.status));
      return data;
    },
    async fetchJournal(tenantKey) {
      try {
        const r = await fetch("/api/journal?tenant=" + encodeURIComponent(tenantKey) + "&limit=60");
        if (!r.ok) return null;
        const e = (await r.json()).events || [];
        journalCache[tenantKey] = e;
        return e;
      } catch (x) { return null; }
    },
    _patch() {
      const C = window.MatrixCorpus;
      if (!C) return;
      const mockGen = C.generate.bind(C);
      const mockComms = C.comms ? C.comms.bind(C) : null;
      C.generate = (tenantKey, count) => cache[tenantKey] || mockGen(tenantKey, count);
      if (mockComms) {
        C.comms = (tenantKey) => {
          const live = commsCache[tenantKey];
          if (live && Array.isArray(live.columns)) return live; // shape match ({columns,agents})
          return mockComms(tenantKey);
        };
      }
    },
  };
})();
