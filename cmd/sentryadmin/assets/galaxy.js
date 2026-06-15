/* MATRIX SENTRY — Vector Galaxy (Three.js r137, global build).
   Exposes window.MatrixGalaxy. Owns the WebGL scene imperatively; React only
   feeds it data + reads back selection/recall via callbacks. */
(function () {
  "use strict";

  const FOG = new THREE.Color(0x05070f);

  function hex(c) { return new THREE.Color(c); }

  const VERT = `
    attribute vec3 aColor;
    attribute float aSize;
    attribute float aHeat;
    attribute float aPhase;
    attribute float aSelect;
    attribute float aAlive;
    uniform float uTime;
    uniform float uPixelRatio;
    uniform float uFocus;
    uniform float uFocusRange;
    uniform float uSizeScale;
    varying vec3 vColor;
    varying float vHeat;
    varying float vBlur;
    varying float vDepth;
    varying float vSelect;
    varying float vAlive;
    void main() {
      vec4 mv = modelViewMatrix * vec4(position, 1.0);
      float depth = -mv.z;
      float pulse = 1.0 + aHeat * 0.55 * sin(uTime * 2.1 + aPhase) + aSelect * 0.8;
      float coc = clamp(abs(depth - uFocus) / uFocusRange, 0.0, 1.0);
      vBlur = coc;
      float size = aSize * uSizeScale * pulse * (1.0 + coc * 1.7) * aAlive;
      gl_PointSize = size * (88.0 / depth) * uPixelRatio;
      gl_Position = projectionMatrix * mv;
      vColor = aColor; vHeat = aHeat; vDepth = depth; vSelect = aSelect; vAlive = aAlive;
    }
  `;

  const FRAG = `
    precision highp float;
    uniform float uFogDensity;
    uniform vec3 uFogColor;
    varying vec3 vColor;
    varying float vHeat;
    varying float vBlur;
    varying float vDepth;
    varying float vSelect;
    varying float vAlive;
    void main() {
      vec2 uv = gl_PointCoord - 0.5;
      float d = length(uv);
      float edge = mix(0.46, 0.12, vBlur);
      float core = smoothstep(0.5, edge, d);
      float glow = smoothstep(0.5, 0.0, d);
      float alpha = core * 0.95 + glow * 0.26;
      vec3 col = vColor * (1.0 + vHeat * 1.7 + vSelect * 1.6);
      col += vSelect * vec3(0.7) * core;
      float fog = 1.0 - exp(-uFogDensity * uFogDensity * vDepth * vDepth);
      col = mix(col, uFogColor, fog * 0.9);
      alpha *= (1.0 - vBlur * 0.5) * (1.0 - fog * 0.72) * vAlive;
      if (alpha < 0.012) discard;
      gl_FragColor = vec4(col, alpha);
    }
  `;

  function haloTexture() {
    const s = 128;
    const cv = document.createElement("canvas");
    cv.width = cv.height = s;
    const g = cv.getContext("2d");
    const grd = g.createRadialGradient(s / 2, s / 2, 0, s / 2, s / 2, s / 2);
    grd.addColorStop(0, "rgba(255,255,255,1)");
    grd.addColorStop(0.25, "rgba(255,255,255,0.55)");
    grd.addColorStop(1, "rgba(255,255,255,0)");
    g.fillStyle = grd;
    g.fillRect(0, 0, s, s);
    const t = new THREE.CanvasTexture(cv);
    return t;
  }

  class Galaxy {
    constructor(canvas, layer, cb) {
      this.canvas = canvas;
      this.layer = layer; // DOM overlay for projected labels
      this.cb = cb || {};
      this.points = [];      // memory objects parallel to geometry verts
      this.offsets = [0];    // per-set x offset
      this.clusters = [];    // {center(Vector3 world), label, color, el}
      this.labelEls = [];
      this.recallEls = [];
      this.hovered = -1;
      this.selectedIdx = -1;
      this.idleT = 0;
      this.tau = 1.5;
      this.disposed = false;
      this._init();
    }

    _init() {
      const w = this.canvas.clientWidth || window.innerWidth;
      const h = this.canvas.clientHeight || window.innerHeight;
      const r = new THREE.WebGLRenderer({ canvas: this.canvas, antialias: true, alpha: false, powerPreference: "high-performance" });
      r.setPixelRatio(Math.min(window.devicePixelRatio, 2));
      r.setSize(w, h, false);
      r.setClearColor(0x05070f, 1);
      this.renderer = r;

      const scene = new THREE.Scene();
      scene.fog = new THREE.FogExp2(0x05070f, 0.012);
      this.scene = scene;

      const cam = new THREE.PerspectiveCamera(54, w / h, 0.1, 400);
      cam.position.set(2, 7, 40);
      this.camera = cam;

      const controls = new THREE.OrbitControls(cam, this.canvas);
      controls.enableDamping = true;
      controls.dampingFactor = 0.06;
      controls.rotateSpeed = 0.55;
      controls.autoRotate = true;
      controls.autoRotateSpeed = 0.35;
      controls.minDistance = 12;
      controls.maxDistance = 120;
      controls.enablePan = false;
      controls.addEventListener("start", () => { controls.autoRotate = false; this.idleT = 0; });
      controls.addEventListener("end", () => { this.idleT = 0; });
      this.controls = controls;

      // postprocessing: bloom
      const composer = new THREE.EffectComposer(r);
      composer.addPass(new THREE.RenderPass(scene, cam));
      const bloom = new THREE.UnrealBloomPass(new THREE.Vector2(w, h), 0.72, 0.5, 0.18);
      bloom.threshold = 0.16;
      bloom.strength = 0.72;
      bloom.radius = 0.5;
      composer.addPass(bloom);
      this.composer = composer;
      this.bloom = bloom;

      // ambient nebula backdrop — large faint additive sprites
      this.nebula = new THREE.Group();
      const halo = haloTexture();
      this.haloTex = halo;
      const neb = [
        [0x0a1838, -16, -8, -42, 70],
        [0x1a0a30, 22, 10, -52, 64],
        [0x06222e, -8, 12, -34, 54],
      ];
      neb.forEach(([c, x, y, z, s]) => {
        const m = new THREE.Sprite(new THREE.SpriteMaterial({ map: halo, color: c, transparent: true, opacity: 0.16, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
        m.position.set(x, y, z); m.scale.set(s, s, 1);
        this.nebula.add(m);
      });
      scene.add(this.nebula);

      this.haloGroup = new THREE.Group();
      scene.add(this.haloGroup);

      // recall lines
      const lineGeo = new THREE.BufferGeometry();
      lineGeo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(0), 3));
      this.recallLines = new THREE.LineSegments(lineGeo, new THREE.LineBasicMaterial({ color: 0x35e6ff, transparent: true, opacity: 0.0, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
      scene.add(this.recallLines);

      // query node
      this.queryNode = new THREE.Mesh(
        new THREE.SphereGeometry(0.55, 20, 20),
        new THREE.MeshBasicMaterial({ color: 0xffffff })
      );
      this.queryNode.visible = false;
      scene.add(this.queryNode);
      const qhalo = new THREE.Sprite(new THREE.SpriteMaterial({ map: halo, color: 0x9fefff, transparent: true, opacity: 0.9, blending: THREE.AdditiveBlending, depthWrite: false }));
      qhalo.scale.set(5, 5, 1);
      this.queryNode.add(qhalo);

      // dedup tau sphere
      this.tauSphere = new THREE.Mesh(
        new THREE.SphereGeometry(1, 24, 24),
        new THREE.MeshBasicMaterial({ color: 0xffb23e, transparent: true, opacity: 0.10, blending: THREE.AdditiveBlending, depthWrite: false, side: THREE.DoubleSide })
      );
      this.tauSphere.visible = false;
      scene.add(this.tauSphere);
      const tauWire = new THREE.Mesh(
        new THREE.SphereGeometry(1, 18, 18),
        new THREE.MeshBasicMaterial({ color: 0xffd27a, transparent: true, opacity: 0.18, wireframe: true, depthWrite: false })
      );
      this.tauSphere.add(tauWire);

      // isolation wall (hidden until split)
      this.wall = this._buildWall();
      this.wall.visible = false;
      scene.add(this.wall);

      this.raycaster = new THREE.Raycaster();
      this.raycaster.params.Points = { threshold: 0.55 };
      this.mouse = new THREE.Vector2(-2, -2);
      this.uniforms = {
        uTime: { value: 0 },
        uPixelRatio: { value: Math.min(window.devicePixelRatio, 2) },
        uFocus: { value: 34 },
        uFocusRange: { value: 26 },
        uSizeScale: { value: 1 },
        uFogDensity: { value: 0.020 },
        uFogColor: { value: FOG.clone() },
      };

      this._bindEvents();
      this.clock = new THREE.Clock();
      this._loop();
    }

    _buildWall() {
      const g = new THREE.Group();
      const W = 26, H = 30;
      const geo = new THREE.PlaneGeometry(W, H, 1, 1);
      const mat = new THREE.ShaderMaterial({
        transparent: true, depthWrite: false, side: THREE.DoubleSide, blending: THREE.AdditiveBlending,
        uniforms: { uTime: { value: 0 } },
        vertexShader: "varying vec2 vUv; void main(){ vUv=uv; gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.0); }",
        fragmentShader: `
          varying vec2 vUv; uniform float uTime;
          void main(){
            vec2 g = abs(fract(vUv * vec2(13.0, 16.0)) - 0.5);
            float grid = smoothstep(0.46, 0.5, max(g.x, g.y));
            float scan = 0.5 + 0.5 * sin((vUv.y * 40.0) - uTime * 2.5);
            float edge = smoothstep(0.0, 0.08, vUv.x) * smoothstep(1.0, 0.92, vUv.x);
            edge *= smoothstep(0.0, 0.06, vUv.y) * smoothstep(1.0, 0.94, vUv.y);
            float a = (grid * 0.22 + scan * 0.05) * (0.35 + 0.65 * edge);
            vec3 col = mix(vec3(0.20,0.90,1.0), vec3(1.0,0.24,0.80), vUv.y);
            gl_FragColor = vec4(col, a * 0.8);
          }
        `,
      });
      const plane = new THREE.Mesh(geo, mat);
      plane.rotation.y = Math.PI / 2; // normal along X
      g.add(plane);
      this.wallMat = mat;
      // frame edges
      const edgeMat = new THREE.LineBasicMaterial({ color: 0x6fe0ff, transparent: true, opacity: 0.5, blending: THREE.AdditiveBlending });
      const hw = W / 2, hh = H / 2;
      const pts = [[-hw, -hh], [hw, -hh], [hw, hh], [-hw, hh], [-hw, -hh]].map((p) => new THREE.Vector3(0, p[1], p[0]));
      g.add(new THREE.Line(new THREE.BufferGeometry().setFromPoints(pts), edgeMat));
      return g;
    }

    _bindEvents() {
      this._onMove = (e) => {
        const rect = this.canvas.getBoundingClientRect();
        this.mouse.x = ((e.clientX - rect.left) / rect.width) * 2 - 1;
        this.mouse.y = -((e.clientY - rect.top) / rect.height) * 2 + 1;
        this._wantPick = true;
        this._lastClient = { x: e.clientX - rect.left, y: e.clientY - rect.top };
        this.controls.autoRotate = false;
        this.idleT = 0;
      };
      this._onClick = () => {
        if (this.hovered >= 0) this._select(this.hovered);
      };
      this._onLeave = () => { this.mouse.set(-2, -2); this._setHover(-1); };
      this.canvas.addEventListener("pointermove", this._onMove);
      this.canvas.addEventListener("pointerdown", this._onMove);
      this.canvas.addEventListener("click", this._onClick);
      this.canvas.addEventListener("pointerleave", this._onLeave);
      this._onResize = () => this.resize();
      window.addEventListener("resize", this._onResize);
    }

    // ---- data ----
    setData(dataA, dataB) {
      this.split = !!dataB;
      this._clearPoints();
      this._clearLabels();
      this.recallActive = false;
      this._setRecallLines([]);
      this.queryNode.visible = false;
      this.tauSphere.visible = false;

      const sets = dataB ? [dataA, dataB] : [dataA];
      const gap = dataB ? 22 : 0;
      this.offsets = dataB ? [-gap, gap] : [0];
      this.wall.visible = !!dataB;

      const all = [];
      const clusters = [];
      sets.forEach((d, si) => {
        const ox = this.offsets[si];
        d.points.forEach((p) => {
          all.push({ mem: p, x: p.pos[0] + ox, y: p.pos[1], z: p.pos[2], side: si });
        });
        d.clusters.forEach((c) => {
          clusters.push({
            label: c.label, color: c.color, side: si,
            world: new THREE.Vector3(c.center[0] + ox, c.center[1], c.center[2]),
          });
        });
      });

      const N = all.length;
      const pos = new Float32Array(N * 3);
      const col = new Float32Array(N * 3);
      const size = new Float32Array(N);
      const heat = new Float32Array(N);
      const phase = new Float32Array(N);
      const sel = new Float32Array(N);
      const alive = new Float32Array(N);
      const c = new THREE.Color();
      this.points = all.map((a) => a.mem);
      this._worldX = new Float32Array(N);
      for (let i = 0; i < N; i++) {
        const a = all[i];
        pos[i * 3] = a.x; pos[i * 3 + 1] = a.y; pos[i * 3 + 2] = a.z;
        this._worldX[i] = a.x;
        c.set(a.mem.color);
        col[i * 3] = c.r; col[i * 3 + 1] = c.g; col[i * 3 + 2] = c.b;
        size[i] = 1.4 + a.mem.heat * 2.6 + Math.random() * 0.4;
        heat[i] = a.mem.heat;
        phase[i] = Math.random() * Math.PI * 2;
        sel[i] = 0;
        // fresh memories (< 1.5 days) burst in
        const ageD = (Date.now() - a.mem.createdAt) / 86400000;
        alive[i] = ageD < 1.5 ? 0.0 : 1.0;
        a.mem._fresh = ageD < 1.5;
      }
      const geo = new THREE.BufferGeometry();
      geo.setAttribute("position", new THREE.BufferAttribute(pos, 3));
      geo.setAttribute("aColor", new THREE.BufferAttribute(col, 3));
      geo.setAttribute("aSize", new THREE.BufferAttribute(size, 1));
      geo.setAttribute("aHeat", new THREE.BufferAttribute(heat, 1));
      geo.setAttribute("aPhase", new THREE.BufferAttribute(phase, 1));
      geo.setAttribute("aSelect", new THREE.BufferAttribute(sel, 1));
      geo.setAttribute("aAlive", new THREE.BufferAttribute(alive, 1));
      const mat = new THREE.ShaderMaterial({
        uniforms: this.uniforms,
        vertexShader: VERT, fragmentShader: FRAG,
        transparent: true, depthWrite: false, depthTest: false,
        blending: THREE.AdditiveBlending,
      });
      this.cloud = new THREE.Points(geo, mat);
      this.cloud.frustumCulled = false;
      this.scene.add(this.cloud);
      this.geo = geo;

      // halos + labels
      clusters.forEach((cl) => {
        const sprite = new THREE.Sprite(new THREE.SpriteMaterial({ map: this.haloTex, color: hex(cl.color), transparent: true, opacity: 0.16, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
        sprite.position.copy(cl.world);
        sprite.scale.set(13, 13, 1);
        this.haloGroup.add(sprite);
        cl._sprite = sprite;
        const el = document.createElement("div");
        el.style.cssText = "position:absolute;left:0;top:0;display:flex;align-items:center;gap:6px;" +
          "font:600 10.5px/1 'Space Grotesk',sans-serif;letter-spacing:.1em;text-transform:uppercase;" +
          "color:#d4e3f6;padding:4px 10px 4px 8px;border-radius:999px;white-space:nowrap;" +
          "background:rgba(8,12,22,0.5);-webkit-backdrop-filter:blur(6px);backdrop-filter:blur(6px);" +
          "border:1px solid rgba(255,255,255,0.09);pointer-events:none;transition:opacity .35s;will-change:transform,opacity;";
        el.innerHTML = '<span style="width:7px;height:7px;border-radius:50%;background:' + cl.color + ';box-shadow:0 0 10px ' + cl.color + '"></span>' + cl.label;
        this.layer.appendChild(el);
        cl.el = el;
      });
      this.clusters = clusters;

      // intro fly-in: lift fresh ones over the first ~2s
      this._freshTimers = [];
      all.forEach((a, i) => {
        if (a.mem._fresh) {
          const t = setTimeout(() => this._reveal(i), 200 + Math.random() * 2200);
          this._freshTimers.push(t);
        }
      });

      // frame camera
      if (this.split) {
        this.controls.target.set(0, 0, 0);
        this.camera.position.set(0, 10, 64);
        this.controls.autoRotate = false;
        this.uniforms.uFocus.value = 60;
        this.uniforms.uFocusRange.value = 40;
      } else {
        this.controls.target.set(0, 0, 0);
        this.camera.position.set(2, 7, 40);
        this.controls.autoRotate = true;
        this.uniforms.uFocus.value = 34;
        this.uniforms.uFocusRange.value = 26;
      }
      this.controls.update();
      if (this.cb.onReady) this.cb.onReady();
    }

    _reveal(i) {
      if (!this.geo) return;
      const a = this.geo.attributes.aAlive;
      a.array[i] = 1; a.needsUpdate = true;
      // brief heat flash
      const h = this.geo.attributes.aHeat;
      const orig = h.array[i];
      h.array[i] = 1.0; h.needsUpdate = true;
      setTimeout(() => { if (this.geo) { h.array[i] = orig; h.needsUpdate = true; } }, 1400);
    }

    _clearPoints() {
      (this._freshTimers || []).forEach(clearTimeout);
      if (this.cloud) { this.scene.remove(this.cloud); this.geo.dispose(); this.cloud.material.dispose(); this.cloud = null; }
      while (this.haloGroup.children.length) {
        const s = this.haloGroup.children.pop();
        this.haloGroup.remove(s); s.material.dispose();
      }
    }
    _clearLabels() {
      this.clusters.forEach((c) => c.el && c.el.remove());
      this.recallEls.forEach((e) => e.remove());
      this.recallEls = [];
      this.clusters = [];
    }

    // ---- interaction ----
    _pick() {
      if (!this.cloud) return -1;
      this.raycaster.setFromCamera(this.mouse, this.camera);
      const hits = this.raycaster.intersectObject(this.cloud);
      if (hits.length) {
        // nearest along ray by distanceToRay
        hits.sort((a, b) => (a.distanceToRay - b.distanceToRay) || (a.distance - b.distance));
        return hits[0].index;
      }
      return -1;
    }

    _setHover(i) {
      if (i === this.hovered) return;
      this.hovered = i;
      this.canvas.style.cursor = i >= 0 ? "pointer" : "grab";
      if (i < 0) {
        this.tauSphere.visible = false;
        this._dupHL(this._lastDup, false);
        this._lastDup = [];
        if (this.cb.onHover) this.cb.onHover(null);
        return;
      }
      const p = this._pos(i);
      this.tauSphere.position.copy(p);
      this.tauSphere.scale.setScalar(this.tau);
      this.tauSphere.visible = true;
      // dedup neighbors within tau
      this._dupHL(this._lastDup, false);
      const dup = this._within(i, this.tau);
      this._dupHL(dup, true);
      this._lastDup = dup;
      const mem = this.points[i];
      if (this.cb.onHover) this.cb.onHover({
        index: i, mem, dup: dup.length,
        x: this._lastClient ? this._lastClient.x : 0,
        y: this._lastClient ? this._lastClient.y : 0,
      });
    }

    _dupHL(list, on) {
      if (!this.geo || !list) return;
      const s = this.geo.attributes.aSelect;
      list.forEach((i) => { if (i !== this.selectedIdx) s.array[i] = on ? 0.6 : 0; });
      s.needsUpdate = true;
    }

    _within(i, r) {
      const p = this._pos(i); const out = []; const r2 = r * r;
      const N = this.points.length;
      const pos = this.geo.attributes.position.array;
      for (let j = 0; j < N; j++) {
        if (j === i) continue;
        const dx = pos[j * 3] - p.x, dy = pos[j * 3 + 1] - p.y, dz = pos[j * 3 + 2] - p.z;
        if (dx * dx + dy * dy + dz * dz < r2) out.push(j);
      }
      return out;
    }

    _pos(i) {
      const a = this.geo.attributes.position.array;
      return new THREE.Vector3(a[i * 3], a[i * 3 + 1], a[i * 3 + 2]);
    }

    _knn(i, k, sameSideOnly) {
      const p = this._pos(i); const N = this.points.length;
      const pos = this.geo.attributes.position.array;
      const side = this.points[i].tenant;
      const arr = [];
      for (let j = 0; j < N; j++) {
        if (j === i) continue;
        if (sameSideOnly && this.points[j].tenant !== side) continue;
        const dx = pos[j * 3] - p.x, dy = pos[j * 3 + 1] - p.y, dz = pos[j * 3 + 2] - p.z;
        arr.push({ j, d: Math.sqrt(dx * dx + dy * dy + dz * dz) });
      }
      arr.sort((a, b) => a.d - b.d);
      return arr.slice(0, k);
    }

    _select(i) {
      if (this.selectedIdx >= 0 && this.geo) { this.geo.attributes.aSelect.array[this.selectedIdx] = 0; }
      this.selectedIdx = i;
      const s = this.geo.attributes.aSelect;
      s.array[i] = 1; s.needsUpdate = true;
      const nn = this._knn(i, 8, true);
      const mem = this.points[i];
      // cosine-ish distance scaled from 3D dist (purely cosmetic)
      const neighbors = nn.map((n) => ({ mem: this.points[n.j], dist: +(n.d * 0.018).toFixed(3) }));
      this._drawLinks(i, nn);
      if (this.cb.onSelect) this.cb.onSelect({ mem, neighbors, index: i });
    }

    selectById(id) {
      const i = this.points.findIndex((p) => p.id === id);
      if (i >= 0) { this._focusCamera(this._pos(i)); this._select(i); }
    }

    _drawLinks(i, nn) {
      const p = this._pos(i);
      const arr = new Float32Array(nn.length * 6);
      nn.forEach((n, k) => {
        const q = this._pos(n.j);
        arr[k * 6] = p.x; arr[k * 6 + 1] = p.y; arr[k * 6 + 2] = p.z;
        arr[k * 6 + 3] = q.x; arr[k * 6 + 4] = q.y; arr[k * 6 + 5] = q.z;
      });
      this.recallLines.geometry.setAttribute("position", new THREE.BufferAttribute(arr, 3));
      this.recallLines.geometry.attributes.position.needsUpdate = true;
      this.recallLines.material.opacity = 0.55;
      this.recallLines.material.color.set(0x35e6ff);
      this.recallActive = true;
      this._recallDist = nn.map((n, k) => {
        const q = this._pos(n.j);
        return { mid: p.clone().lerp(q, 0.5), d: +(n.d * 0.018).toFixed(3) };
      });
      this._renderRecallLabels();
    }

    runRecall(query) {
      if (!this.cloud) return;
      // choose target: cluster whose label best matches query tokens, else hottest
      const q = (query || "").toLowerCase();
      let best = null, bestScore = -1;
      this.clusters.forEach((c) => {
        let sc = 0;
        if (q && (q.includes(c.label) || c.label.includes(q))) sc += 5;
        sc += Math.random();
        if (sc > bestScore) { bestScore = sc; best = c; }
      });
      const target = best ? best.world.clone() : new THREE.Vector3();
      target.x += (Math.random() - 0.5) * 4;
      target.y += (Math.random() - 0.5) * 4;
      target.z += (Math.random() - 0.5) * 4;

      // nearest index to target = pseudo query vector landing
      let qi = 0, qd = Infinity; const pos = this.geo.attributes.position.array;
      for (let j = 0; j < this.points.length; j++) {
        const dx = pos[j * 3] - target.x, dy = pos[j * 3 + 1] - target.y, dz = pos[j * 3 + 2] - target.z;
        const d = dx * dx + dy * dy + dz * dz; if (d < qd) { qd = d; qi = j; }
      }
      // fly query node in
      const from = this.camera.position.clone().lerp(target, 0.15);
      this.queryNode.position.copy(from);
      this.queryNode.visible = true;
      this._animQuery = { from, to: target, t: 0, qi };
      this._focusCamera(target);
      return qi;
    }

    _animateQueryStep(dt) {
      if (!this._animQuery) return;
      const a = this._animQuery; a.t = Math.min(1, a.t + dt * 1.6);
      const e = 1 - Math.pow(1 - a.t, 3);
      this.queryNode.position.lerpVectors(a.from, a.to, e);
      if (a.t >= 1) {
        const i = a.qi;
        this._animQuery = null;
        const nn = this._knn(i, 8, true);
        // draw lines from query node to neighbors
        const p = this.queryNode.position.clone();
        const arr = new Float32Array(nn.length * 6);
        nn.forEach((n, k) => {
          const qv = this._pos(n.j);
          arr[k * 6] = p.x; arr[k * 6 + 1] = p.y; arr[k * 6 + 2] = p.z;
          arr[k * 6 + 3] = qv.x; arr[k * 6 + 4] = qv.y; arr[k * 6 + 5] = qv.z;
          // pulse neighbor heat
          this._flashHeat(n.j);
        });
        this.recallLines.geometry.setAttribute("position", new THREE.BufferAttribute(arr, 3));
        this.recallLines.geometry.attributes.position.needsUpdate = true;
        this.recallLines.material.opacity = 0.6;
        this.recallActive = true;
        this._recallDist = nn.map((n) => {
          const qv = this._pos(n.j);
          return { mid: p.clone().lerp(qv, 0.5), d: +(n.d * 0.018).toFixed(3) };
        });
        this._renderRecallLabels();
        const neighbors = nn.map((n) => ({ mem: this.points[n.j], dist: +(n.d * 0.018).toFixed(3) }));
        if (this.cb.onRecall) this.cb.onRecall({ neighbors, index: i, mem: this.points[i] });
      }
    }

    _flashHeat(i) {
      const h = this.geo.attributes.aHeat; const orig = h.array[i];
      h.array[i] = 1.0; h.needsUpdate = true;
      setTimeout(() => { if (this.geo) { h.array[i] = orig; h.needsUpdate = true; } }, 1800);
    }

    forget(id) {
      const i = this.points.findIndex((p) => p.id === id);
      if (i < 0 || !this.geo) return;
      const a = this.geo.attributes.aAlive;
      const start = performance.now();
      const dur = 700;
      const tick = () => {
        const t = Math.min(1, (performance.now() - start) / dur);
        a.array[i] = 1 - t; a.needsUpdate = true;
        if (t < 1) requestAnimationFrame(tick);
      };
      tick();
      if (i === this.selectedIdx) { this._clearSelection(); }
    }
    promote(id) {
      const i = this.points.findIndex((p) => p.id === id);
      if (i < 0) return;
      this._flashHeat(i);
      const sz = this.geo.attributes.aSize;
      sz.array[i] = Math.min(5, sz.array[i] + 1.4); sz.needsUpdate = true;
    }
    _clearSelection() {
      if (this.selectedIdx >= 0 && this.geo) { this.geo.attributes.aSelect.array[this.selectedIdx] = 0; this.geo.attributes.aSelect.needsUpdate = true; }
      this.selectedIdx = -1;
      this.recallActive = false;
      this._setRecallLines([]);
    }
    clearRecall() {
      this.recallActive = false;
      this.recallLines.material.opacity = 0;
      this.queryNode.visible = false;
      this._recallDist = [];
      this._renderRecallLabels();
    }
    _setRecallLines() {
      this.recallLines.material.opacity = 0;
      this._recallDist = [];
      this._renderRecallLabels();
    }

    _renderRecallLabels() {
      const need = (this._recallDist || []).length;
      while (this.recallEls.length < need) {
        const el = document.createElement("div");
        el.style.cssText = "position:absolute;left:0;top:0;font:600 10px 'JetBrains Mono',monospace;" +
          "color:#9ff0ff;background:rgba(5,12,20,0.72);padding:2px 6px;border-radius:6px;" +
          "border:1px solid rgba(53,230,255,0.32);pointer-events:none;white-space:nowrap;" +
          "box-shadow:0 0 12px rgba(53,230,255,0.28);will-change:transform,opacity;";
        this.layer.appendChild(el);
        this.recallEls.push(el);
      }
      while (this.recallEls.length > need) this.recallEls.pop().remove();
      (this._recallDist || []).forEach((d, k) => { this.recallEls[k].textContent = d.d.toFixed(3); });
    }

    _focusCamera(target) {
      this._camTween = { to: target.clone(), t: 0 };
    }

    setFocusValue(v) { this.uniforms.uFocus.value = v; }

    resize() {
      if (this.disposed) return;
      const w = this.canvas.clientWidth || window.innerWidth;
      const h = this.canvas.clientHeight || window.innerHeight;
      this.renderer.setSize(w, h, false);
      this.composer.setSize(w, h);
      this.bloom.setSize(w, h);
      this.camera.aspect = w / h; this.camera.updateProjectionMatrix();
    }

    _projectLabels() {
      const w = this.canvas.clientWidth, h = this.canvas.clientHeight;
      const v = new THREE.Vector3();
      this.clusters.forEach((c) => {
        v.copy(c.world).project(this.camera);
        const behind = v.z > 1;
        const x = (v.x * 0.5 + 0.5) * w, y = (-v.y * 0.5 + 0.5) * h;
        const dist = this.camera.position.distanceTo(c.world);
        const op = behind ? 0 : Math.max(0, Math.min(1, (95 - dist) / 55));
        c.el.style.transform = "translate(-50%,-50%) translate(" + x + "px," + y + "px)";
        c.el.style.opacity = op;
      });
      (this._recallDist || []).forEach((d, k) => {
        if (!this.recallEls[k]) return;
        v.copy(d.mid).project(this.camera);
        const behind = v.z > 1;
        const x = (v.x * 0.5 + 0.5) * w, y = (-v.y * 0.5 + 0.5) * h;
        this.recallEls[k].style.transform = "translate(-50%,-50%) translate(" + x + "px," + y + "px)";
        this.recallEls[k].style.opacity = behind ? 0 : 1;
      });
    }

    _loop() {
      if (this.disposed) return;
      requestAnimationFrame(() => this._loop());
      const dt = Math.min(0.05, this.clock.getDelta());
      this.uniforms.uTime.value += dt;
      if (this.wallMat) this.wallMat.uniforms.uTime.value += dt;
      this.idleT += dt;
      if (this.idleT > 3.5 && !this.split) this.controls.autoRotate = true;

      if (this._wantPick) { this._wantPick = false; this._setHover(this._pick()); }
      this._animateQueryStep(dt);

      if (this._camTween) {
        this._camTween.t = Math.min(1, this._camTween.t + dt * 1.2);
        const e = 1 - Math.pow(1 - this._camTween.t, 3);
        this.controls.target.lerp(this._camTween.to, e * 0.12);
        if (this._camTween.t >= 1) this._camTween = null;
      }

      // gentle nebula drift
      this.nebula.rotation.y += dt * 0.01;

      this.controls.update();
      this._projectLabels();
      this.composer.render();
    }

    dispose() {
      this.disposed = true;
      (this._freshTimers || []).forEach(clearTimeout);
      window.removeEventListener("resize", this._onResize);
      this.canvas.removeEventListener("pointermove", this._onMove);
      this.canvas.removeEventListener("pointerdown", this._onMove);
      this.canvas.removeEventListener("click", this._onClick);
      this.canvas.removeEventListener("pointerleave", this._onLeave);
      this._clearLabels();
      try { this.renderer.dispose(); } catch (e) {}
    }
  }

  window.MatrixGalaxy = Galaxy;
})();
