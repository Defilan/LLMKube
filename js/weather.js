// Weather: rain on glass, wind state, time of night.
// Rain is a surface effect on the glass panes, not falling geometry.
// Attached to window so other files can use it without a module system.
(function () {
  'use strict';

  /* ---- Weather ---- */
  var Weather = function (scene, camera, renderer) {
    this.scene = scene;
    this.camera = camera;
    this.renderer = renderer;

    // Time of night: 0 = midnight, 0.5 = dawn, 1.0 = midnight again
    this.timeOfNight = 0.0;

    // Wind state
    this.wind = new THREE.Vector3();
    this.windTarget = new THREE.Vector3();
    this.windDecay = 0.95;

    // Rain droplets on glass (surface effect)
    this.droplets = [];
    this.maxDroplets = 300;
    this.dropletPool = [];

    // Pre-allocate droplet pool
    for (var i = 0; i < this.maxDroplets; i++) {
      this.dropletPool.push({
        active: false,
        x: 0, y: 0, z: 0,       // position on glass
        speed: 0,               // fall speed
        size: 0,                // visual size
        trail: 0,               // trail length
        life: 0,                // time alive
        maxLife: 5,             // max lifetime
        pane: 0                 // which glass pane
      });
    }

    // Rain texture: small circle for each droplet
    this.rainTexture = this.createRainTexture();
    this.rainMaterial = new THREE.PointsMaterial({
      map: this.rainTexture,
      size: 0.08,
      transparent: true,
      opacity: 0.7,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
      color: 0x88ccdd
    });

    this.rainGeometry = new THREE.BufferGeometry();
    this.rainPositions = new Float32Array(this.maxDroplets * 3);
    this.rainGeometry.setAttribute('position', new THREE.BufferAttribute(this.rainPositions, 3));

    this.rainPoints = new THREE.Points(this.rainGeometry, this.rainMaterial);
    this.scene.add(this.rainPoints);
  };

  Weather.prototype.createRainTexture = function () {
    var canvas = document.createElement('canvas');
    canvas.width = 32;
    canvas.height = 32;
    var ctx = canvas.getContext('2d');

    // Draw a teardrop shape
    var gradient = ctx.createRadialGradient(16, 12, 0, 16, 16, 16);
    gradient.addColorStop(0, 'rgba(200, 230, 255, 1)');
    gradient.addColorStop(0.5, 'rgba(150, 200, 230, 0.6)');
    gradient.addColorStop(1, 'rgba(100, 150, 200, 0)');
    ctx.fillStyle = gradient;
    ctx.fillRect(0, 0, 32, 32);

    var texture = new THREE.CanvasTexture(canvas);
    return texture;
  };

  Weather.prototype.acquireDroplet = function () {
    for (var i = 0; i < this.maxDroplets; i++) {
      if (!this.dropletPool[i].active) {
        this.dropletPool[i].active = true;
        return this.dropletPool[i];
      }
    }
    return null;
  };

  Weather.prototype.releaseDroplet = function (d) {
    d.active = false;
  };

  Weather.prototype.spawnDroplet = function () {
    var d = this.acquireDroplet();
    if (!d) return;

    // Pick a random glass pane
    var pane = Math.floor(Math.random() * 6); // 6 glass surfaces
    d.pane = pane;

    // Position on the pane
    switch (pane) {
      case 0: // back wall
        d.x = (Math.random() - 0.5) * 6;
        d.y = 0.5 + Math.random() * 3.5;
        d.z = -3.99;
        break;
      case 1: // front wall
        d.x = (Math.random() - 0.5) * 6;
        d.y = 0.5 + Math.random() * 3.5;
        d.z = 3.99;
        break;
      case 2: // left wall
        d.x = -2.99;
        d.y = 0.5 + Math.random() * 3.5;
        d.z = (Math.random() - 0.5) * 8;
        break;
      case 3: // right wall
        d.x = 2.99;
        d.y = 0.5 + Math.random() * 3.5;
        d.z = (Math.random() - 0.5) * 8;
        break;
      case 4: // roof left
        d.x = (Math.random() - 0.5) * 6;
        d.y = 4.5 + Math.random() * 1.5;
        d.z = -2 + Math.random() * 2;
        break;
      case 5: // roof right
        d.x = (Math.random() - 0.5) * 6;
        d.y = 4.5 + Math.random() * 1.5;
        d.z = 2 - Math.random() * 2;
        break;
    }

    d.speed = 0.3 + Math.random() * 0.5;
    d.size = 0.04 + Math.random() * 0.04;
    d.trail = 0.02 + Math.random() * 0.03;
    d.life = 0;
    d.maxLife = 3 + Math.random() * 4;
  };

  Weather.prototype.setWind = function (x, y, z) {
    this.windTarget.set(x, y, z);
  };

  Weather.prototype.getWind = function () {
    return this.wind;
  };

  Weather.prototype.setTimeOfNight = function (t) {
    this.timeOfNight = Math.max(0, Math.min(1, t));
  };

  Weather.prototype.getTimeOfNight = function () {
    return this.timeOfNight;
  };

  Weather.prototype.update = function (delta, time) {
    // Wind decay toward target
    this.wind.lerp(this.windTarget, 0.1);
    // Decay wind when no input
    this.wind.multiplyScalar(this.windDecay);

    // Update rain droplets
    var spawnRate = 0.05 + this.timeOfNight * 0.1; // more rain at night
    if (Math.random() < spawnRate) {
      this.spawnDroplet();
    }

    for (var i = 0; i < this.maxDroplets; i++) {
      var d = this.dropletPool[i];
      if (!d.active) continue;

      d.life += delta;
      if (d.life >= d.maxLife) {
        this.releaseDroplet(d);
        continue;
      }

      // Droplet falls down the glass
      d.y -= d.speed * delta;

      // Wind pushes droplets sideways
      d.x += this.wind.x * delta * 0.3;
      d.z += this.wind.z * delta * 0.3;

      // Remove if below floor
      if (d.y < 0.1) {
        this.releaseDroplet(d);
      }
    }

    // Update rain buffer
    var count = 0;
    for (var i = 0; i < this.maxDroplets; i++) {
      var d = this.dropletPool[i];
      if (d.active) {
        this.rainPositions[count * 3]     = d.x;
        this.rainPositions[count * 3 + 1] = d.y;
        this.rainPositions[count * 3 + 2] = d.z;
        count++;
      }
    }

    this.rainGeometry.attributes.position.needsUpdate = true;
    this.rainGeometry.setDrawRange(0, count);

    // Adjust scene lighting based on time of night
    this.updateLighting();
  };

  Weather.prototype.updateLighting = function () {
    var t = this.timeOfNight;

    // Fog density: thicker at night
    var fogDensity = 0.04 + t * 0.06;
    this.scene.fog = new THREE.FogExp2(0x05080c, fogDensity);

    // Background color shifts
    var bgR = 0.02 + (1 - t) * 0.03;
    var bgG = 0.03 + (1 - t) * 0.04;
    var bgB = 0.05 + (1 - t) * 0.05;
    this.scene.background = new THREE.Color(bgR, bgG, bgB);
  };

  /* ---- expose ---- */
  window.Weather = Weather;
})();
