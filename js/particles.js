// Particles: object-pooled pollen and burst effects.
// Attached to window so other files can use it without a module system.
(function () {
  'use strict';

  var PALETTE = {
    amber:     0xffaa33,
    warmAmber: 0xffcc44,
    magenta:   0xcc44aa,
    teal:      0x0a6e6e
  };

  /* ---- Particle pool ---- */
  // Each particle is an object with reusable fields. No allocation per frame.
  var Particle = function () {
    this.active = false;
    this.pos = new THREE.Vector3();
    this.vel = new THREE.Vector3();
    this.life = 0;
    this.maxLife = 1;
    this.size = 0.05;
    this.color = new THREE.Color(PALETTE.amber);
  };

  /* ---- ParticleSystem ---- */
  var ParticleSystem = function (scene, maxParticles) {
    this.scene = scene;
    this.maxParticles = maxParticles || 2000;
    this.pool = [];
    this.activeCount = 0;

    // Pre-allocate pool
    for (var i = 0; i < this.maxParticles; i++) {
      this.pool.push(new Particle());
    }

    // Points geometry (reused)
    this.geometry = new THREE.BufferGeometry();
    this.positions = new Float32Array(this.maxParticles * 3);
    this.colors = new Float32Array(this.maxParticles * 3);
    this.sizes = new Float32Array(this.maxParticles);
    this.geometry.setAttribute('position', new THREE.BufferAttribute(this.positions, 3));
    this.geometry.setAttribute('color', new THREE.BufferAttribute(this.colors, 3));
    this.geometry.setAttribute('size', new THREE.BufferAttribute(this.sizes, 1));

    // Shader material for sized points with additive blending
    this.material = new THREE.ShaderMaterial({
      uniforms: {
        uTime: { value: 0 }
      },
      vertexShader: [
        'attribute float size;',
        'attribute vec3 color;',
        'varying vec3 vColor;',
        'void main() {',
        '  vColor = color;',
        '  vec4 mvPos = modelViewMatrix * vec4(position, 1.0);',
        '  gl_PointSize = size * (300.0 / -mvPos.z);',
        '  gl_Position = projectionMatrix * mvPos;',
        '}'
      ].join('\n'),
      fragmentShader: [
        'varying vec3 vColor;',
        'void main() {',
        '  float d = length(gl_PointCoord - vec2(0.5));',
        '  if (d > 0.5) discard;',
        '  float alpha = 1.0 - smoothstep(0.2, 0.5, d);',
        '  gl_FragColor = vec4(vColor, alpha);',
        '}'
      ].join('\n'),
      transparent: true,
      depthWrite: false,
      blending: THREE.AdditiveBlending
    });

    this.points = new THREE.Points(this.geometry, this.material);
    this.scene.add(this.points);
  };

  ParticleSystem.prototype.acquire = function () {
    for (var i = 0; i < this.maxParticles; i++) {
      if (!this.pool[i].active) {
        this.pool[i].active = true;
        this.activeCount++;
        return this.pool[i];
      }
    }
    return null;
  };

  ParticleSystem.prototype.release = function (p) {
    p.active = false;
    this.activeCount--;
  };

  ParticleSystem.prototype.emit = function (origin, count, config) {
    for (var i = 0; i < count; i++) {
      var p = this.acquire();
      if (!p) break;

      p.pos.copy(origin);
      p.pos.x += (Math.random() - 0.5) * (config.spread || 0.5);
      p.pos.y += (Math.random() - 0.5) * (config.spread || 0.5);
      p.pos.z += (Math.random() - 0.5) * (config.spread || 0.5);

      p.vel.set(
        (Math.random() - 0.5) * (config.speed || 1.0),
        (Math.random() - 0.5) * (config.speed || 1.0),
        (Math.random() - 0.5) * (config.speed || 1.0)
      );

      p.life = 0;
      p.maxLife = (config.life || 3.0) * (0.5 + Math.random() * 0.5);
      p.size = (config.size || 0.05) * (0.5 + Math.random() * 0.5);

      if (config.color) {
        p.color.copy(config.color);
      } else {
        p.color.setHex(PALETTE.amber);
      }
    }
  };

  ParticleSystem.prototype.update = function (delta, time, wind) {
    // Wind vector (default zero)
    var wx = wind ? wind.x : 0;
    var wy = wind ? wind.y : 0;
    var wz = wind ? wind.z : 0;

    for (var i = 0; i < this.maxParticles; i++) {
      var p = this.pool[i];
      if (!p.active) continue;

      p.life += delta;
      if (p.life >= p.maxLife) {
        this.release(p);
        continue;
      }

      // Apply wind
      p.vel.x += wx * delta;
      p.vel.y += wy * delta;
      p.vel.z += wz * delta;

      // Damping
      p.vel.multiplyScalar(0.98);

      // Move
      p.pos.x += p.vel.x * delta;
      p.pos.y += p.vel.y * delta;
      p.pos.z += p.vel.z * delta;
    }

    // Update buffer attributes
    for (var i = 0; i < this.maxParticles; i++) {
      var p = this.pool[i];
      var idx = i * 3;
      if (p.active) {
        this.positions[idx]     = p.pos.x;
        this.positions[idx + 1] = p.pos.y;
        this.positions[idx + 2] = p.pos.z;
        this.colors[idx]       = p.color.r;
        this.colors[idx + 1]   = p.color.g;
        this.colors[idx + 2]   = p.color.b;
        // Fade with life
        var lifeRatio = 1.0 - (p.life / p.maxLife);
        this.sizes[i] = p.size * lifeRatio;
      } else {
        this.sizes[i] = 0;
      }
    }

    this.geometry.attributes.position.needsUpdate = true;
    this.geometry.attributes.color.needsUpdate = true;
    this.geometry.attributes.size.needsUpdate = true;
    this.material.uniforms.uTime.value = time;
  };

  /* ---- Firefly ---- */
  // Independent behaviour: wander, land on plants, take off, glow near cursor.
  var Firefly = function (scene, plantSystem) {
    this.scene = scene;
    this.plantSystem = plantSystem;

    // State
    this.state = 'wandering'; // wandering | landing | resting | takingOff
    this.restTimer = 0;
    this.restDuration = 2 + Math.random() * 4;

    // Position and velocity
    this.pos = new THREE.Vector3(
      (Math.random() - 0.5) * 8,
      1 + Math.random() * 3,
      (Math.random() - 0.5) * 10
    );
    this.vel = new THREE.Vector3();
    this.target = new THREE.Vector3();

    // Wander parameters
    this.wanderAngle = Math.random() * Math.PI * 2;
    this.wanderSpeed = 0.3 + Math.random() * 0.4;
    this.wanderRadius = 0.5 + Math.random() * 1.0;

    // Glow
    this.glowIntensity = 0.5 + Math.random() * 0.5;
    this.glowPhase = Math.random() * Math.PI * 2;

    // Mesh: a small glowing sphere
    var geo = new THREE.SphereGeometry(0.03, 6, 4);
    var mat = new THREE.MeshStandardMaterial({
      color: PALETTE.warmAmber,
      emissive: PALETTE.warmAmber,
      emissiveIntensity: this.glowIntensity,
      roughness: 0.2,
      metalness: 0.1
    });
    this.mesh = new THREE.Mesh(geo, mat);
    this.mesh.position.copy(this.pos);
    this.scene.add(this.mesh);

    // Point light for glow
    this.light = new THREE.PointLight(PALETTE.warmAmber, 2, 3, 2);
    this.light.position.copy(this.pos);
    this.scene.add(this.light);

    // Target plant for landing
    this.targetPlant = null;
  };

  Firefly.prototype.update = function (delta, time, cursorWorld) {
    // Glow pulsing
    var glow = this.glowIntensity + Math.sin(time * 2 + this.glowPhase) * 0.3;
    this.mesh.material.emissiveIntensity = Math.max(0.1, glow);
    this.light.intensity = Math.max(0.5, glow * 2);

    // Brighten near cursor
    if (cursorWorld) {
      var distToCursor = this.pos.distanceTo(cursorWorld);
      if (distToCursor < 2) {
        var boost = 1.0 - (distToCursor / 2);
        this.mesh.material.emissiveIntensity += boost * 2;
        this.light.intensity += boost * 3;
      }
    }

    switch (this.state) {
      case 'wandering':
        this.wander(delta, time);
        // Occasionally decide to land
        if (Math.random() < 0.002) {
          this.tryLand();
        }
        break;

      case 'landing':
        this.land(delta);
        break;

      case 'resting':
        this.restTimer -= delta;
        if (this.restTimer <= 0) {
          this.state = 'takingOff';
        }
        break;

      case 'takingOff':
        this.takeOff(delta);
        break;
    }

    // Keep within bounds
    this.pos.x = Math.max(-5, Math.min(5, this.pos.x));
    this.pos.y = Math.max(0.3, Math.min(5, this.pos.y));
    this.pos.z = Math.max(-5, Math.min(5, this.pos.z));

    this.mesh.position.copy(this.pos);
    this.light.position.copy(this.pos);
  };

  Firefly.prototype.wander = function (delta, time) {
    // Slowly change wander angle
    this.wanderAngle += (Math.random() - 0.5) * 0.5 * delta;

    // Target a point in the wander circle
    var tx = this.pos.x + Math.cos(this.wanderAngle) * this.wanderRadius;
    var ty = this.pos.y + Math.sin(time * 0.3 + this.glowPhase) * 0.3;
    var tz = this.pos.z + Math.sin(this.wanderAngle) * this.wanderRadius;

    this.target.set(tx, ty, tz);

    // Move toward target
    var dir = new THREE.Vector3().subVectors(this.target, this.pos);
    var dist = dir.length();
    if (dist > 0.01) {
      dir.normalize();
      this.vel.lerp(dir.multiplyScalar(this.wanderSpeed), 0.05);
    }

    this.pos.x += this.vel.x * delta;
    this.pos.y += this.vel.y * delta;
    this.pos.z += this.vel.z * delta;
  };

  Firefly.prototype.tryLand = function () {
    if (!this.plantSystem || this.plantSystem.plants.length === 0) return;

    // Pick a random plant
    var plant = this.plantSystem.plants[Math.floor(Math.random() * this.plantSystem.plants.length)];
    if (!plant) return;

    this.targetPlant = plant;
    this.state = 'landing';

    // Target the top of the plant
    this.target.copy(plant.position);
    this.target.y += plant.targetHeight * 0.8;
  };

  Firefly.prototype.land = function (delta) {
    if (!this.targetPlant) {
      this.state = 'wandering';
      return;
    }

    var dir = new THREE.Vector3().subVectors(this.target, this.pos);
    var dist = dir.length();

    if (dist > 0.05) {
      dir.normalize();
      this.vel.lerp(dir.multiplyScalar(this.wanderSpeed * 0.5), 0.1);
      this.pos.x += this.vel.x * delta;
      this.pos.y += this.vel.y * delta;
      this.pos.z += this.vel.z * delta;
    } else {
      this.state = 'resting';
      this.restTimer = this.restDuration;
    }
  };

  Firefly.prototype.takeOff = function (delta) {
    // Fly upward and away
    this.vel.y += 0.5 * delta;
    this.vel.x += (Math.random() - 0.5) * 0.3 * delta;
    this.vel.z += (Math.random() - 0.5) * 0.3 * delta;

    this.pos.x += this.vel.x * delta;
    this.pos.y += this.vel.y * delta;
    this.pos.z += this.vel.z * delta;

    this.vel.multiplyScalar(0.95);

    if (this.pos.y > 2) {
      this.state = 'wandering';
      this.targetPlant = null;
    }
  };

  /* ---- FireflyManager ---- */
  var FireflyManager = function (scene, plantSystem, count) {
    this.fireflies = [];
    for (var i = 0; i < (count || 8); i++) {
      this.fireflies.push(new Firefly(scene, plantSystem));
    }
  };

  FireflyManager.prototype.update = function (delta, time, cursorWorld) {
    for (var i = 0; i < this.fireflies.length; i++) {
      this.fireflies[i].update(delta, time, cursorWorld);
    }
  };

  /* ---- expose ---- */
  window.ParticleSystem = ParticleSystem;
  window.FireflyManager = FireflyManager;
})();
