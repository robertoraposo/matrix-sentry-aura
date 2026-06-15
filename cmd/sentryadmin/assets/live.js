/* Live-data shim: replaces window.MatrixCorpus.generate/comms with backend data
   when /api/galaxy is reachable, falling back to the original mock otherwise. */
(function () {
  const cache = {};            // tenantKey -> galaxy data
  const commsCache = {};
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
        if (!this._patched) this._patch();
        this._patched = true;
        console.info("[live] primed", tenantKey, "points:", (cache[tenantKey].points || []).length);
      } catch (e) {
        console.warn("[live] prime failed for", tenantKey, "— using mock:", e.message);
      }
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
          if (live && Array.isArray(live.messages)) return live;
          return mockComms(tenantKey);
        };
      }
    },
  };
})();
