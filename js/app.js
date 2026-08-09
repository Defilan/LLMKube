// App: renderer, scene, camera, controls, lighting, and the render loop.
// Owns bootstrap. Later stages attach their systems here rather than editing
// Greenhouse.
(function () {
  'use strict';

  var Greenhouse = window.Greenhouse;
  var PlantSystem = window.PlantSystem;
  var ParticleSystem = window.ParticleSystem;
  var FireflyManager = window.FireflyManager;
  var Weather = window.Weather;

  var App = function () {
    this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    this.renderer.setSize(window.innerWidth, window.innerHeight);
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.0;
    document.body.appendChild(this.renderer.domElement);

    this.scene = new THREE.Scene();
    this.scene.fog = new THREE.FogExp2(0x05080c, 0.06);

    this.camera = new THREE.PerspectiveCamera(
      55, window.innerWidth / window.innerHeight, 0.1, 200
    );
    this.camera.position.set(0, 2.5, 7);

    this.clock = new THREE.Clock();
    this.frames = 0;

    // Input state
    this.mouse = { x: 0, y: 0, worldX: 0, worldY: 0 };
    this.mouseDown = false;
    this.dragStart = { x: 0, y: 0 };
    this.cursorWorld = new THREE.Vector3();
    this.raycaster = new THREE.Raycaster();
    this._rayPlane = new THREE.Plane(new THREE.Vector3(0, 0, 1), 0);
    this._rayTarget = new THREE.Vector3();
    this._pollenPos = new THREE.Vector3();
    this._pollenColor = new THREE.Color(0xffaa33);
    this._mouseVec2 = new THREE.Vector2();

    // Time of night
    this.timeOfNight = 0.0;
  };

  App.prototype.init = function () {
    this.buildLighting();
    this.buildGreenhouse();
    this.buildPlants();
    this.buildParticles();
    this.buildFireflies();
    this.buildWeather();
    this.setupControls();
    this.setupResize();
    this.setupInput();
  };

  App.prototype.buildPlants = function () {
    this.plantSystem = new PlantSystem(this.scene);
    this.plantSystem.init();
  };

  App.prototype.buildParticles = function () {
    this.particleSystem = new ParticleSystem(this.scene, 2000);
  };

  App.prototype.buildFireflies = function () {
    this.fireflyManager = new FireflyManager(this.scene, this.plantSystem, 8);
  };

  App.prototype.buildWeather = function () {
    this.weather = new Weather(this.scene, this.camera, this.renderer);
  };

  App.prototype.buildLighting = function () {
    // Ambient: deep teal fill
    var ambient = new THREE.AmbientLight(0x0a2a3a, 0.3);
    this.scene.add(ambient);

    // Moonlight: cool indigo directional from above
    var moon = new THREE.DirectionalLight(0x2a1a5e, 0.6);
    moon.position.set(3, 10, -5);
    this.scene.add(moon);

    // Warm amber interior pool lights
    var warm1 = new THREE.PointLight(0xffaa33, 12, 15, 2);
    warm1.position.set(0, 3.5, 0);
    this.scene.add(warm1);

    var warm2 = new THREE.PointLight(0xffcc44, 8, 12, 2);
    warm2.position.set(-2, 3, -3);
    this.scene.add(warm2);

    var warm3 = new THREE.PointLight(0xffcc44, 8, 12, 2);
    warm3.position.set(2, 3, 3);
    this.scene.add(warm3);

    // Soft magenta accent
    var magenta = new THREE.PointLight(0xcc44aa, 6, 10, 2);
    magenta.position.set(0, 1.5, -3.5);
    this.scene.add(magenta);

    // Amber accent near floor
    var amber = new THREE.PointLight(0xffaa33, 6, 8, 2);
    amber.position.set(0, 0.5, 0);
    this.scene.add(amber);

    // Teal fill from below
    var teal = new THREE.PointLight(0x0a6e6e, 4, 10, 2);
    teal.position.set(0, 0.3, 2);
    this.scene.add(teal);

    // Indigo accent
    var indigo = new THREE.PointLight(0x2a1a5e, 5, 10, 2);
    indigo.position.set(0, 2, 3.5);
    this.scene.add(indigo);
  };

  App.prototype.buildGreenhouse = function () {
    this.greenhouse = new Greenhouse(this.scene);
    this.greenhouse.build();
  };

  App.prototype.setupControls = function () {
    this.controls = new THREE.OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.08;
    this.controls.target.set(0, 2, 0);
    this.controls.minDistance = 2;
    this.controls.maxDistance = 15;
    this.controls.minPolarAngle = 0.1;
    this.controls.maxPolarAngle = Math.PI / 2 - 0.05;
    this.controls.maxAzimuthAngle = Math.PI * 0.45;
    this.controls.minAzimuthAngle = -Math.PI * 0.45;
    this.controls.update();
  };

  App.prototype.setupResize = function () {
    var self = this;
    addEventListener('resize', function () {
      self.camera.aspect = window.innerWidth / window.innerHeight;
      self.camera.updateProjectionMatrix();
      self.renderer.setSize(window.innerWidth, window.innerHeight);
    }, { passive: true });
  };

  App.prototype.setupInput = function () {
    var self = this;
    var canvas = self.renderer.domElement;

    // Mouse move: track cursor position and create wind on drag
    canvas.addEventListener('mousemove', function (e) {
      self.mouse.x = (e.clientX / window.innerWidth) * 2 - 1;
      self.mouse.y = -(e.clientY / window.innerHeight) * 2 + 1;

      if (self.mouseDown) {
        var dx = e.clientX - self.dragStart.x;
        var dy = e.clientY - self.dragStart.y;
        self.weather.setWind(dx * 0.001, dy * 0.001, 0);
        self.dragStart.x = e.clientX;
        self.dragStart.y = e.clientY;
      }
    }, { passive: true });

    // Mouse down: start drag
    canvas.addEventListener('mousedown', function (e) {
      self.mouseDown = true;
      self.dragStart.x = e.clientX;
      self.dragStart.y = e.clientY;
    }, { passive: true });

    // Mouse up: stop drag, reset wind
    canvas.addEventListener('mouseup', function () {
      self.mouseDown = false;
      self.weather.setWind(0, 0, 0);
    }, { passive: true });

    // Click: nurture plant
    canvas.addEventListener('click', function (e) {
      self.raycaster.setFromCamera(
        new THREE.Vector2(self.mouse.x, self.mouse.y),
        self.camera
      );
      self.nurturePlant();
    }, { passive: true });

    // Scroll: change time of night
    canvas.addEventListener('wheel', function (e) {
      e.preventDefault();
      self.timeOfNight += e.deltaY * 0.0003;
      self.timeOfNight = Math.max(0, Math.min(1, self.timeOfNight));
      self.weather.setTimeOfNight(self.timeOfNight);
    }, { passive: false });
  };

  App.prototype.nurturePlant = function () {
    // Raycast against plant groups
    var plantGroups = [];
    for (var i = 0; i < this.plantSystem.plants.length; i++) {
      plantGroups.push(this.plantSystem.plants[i].group);
    }

    var intersects = this.raycaster.intersectObjects(plantGroups, true);
    if (intersects.length === 0) return;

    // Find which plant was hit
    var hitObj = intersects[0].object;
    var hitPlant = null;
    for (var i = 0; i < this.plantSystem.plants.length; i++) {
      if (this.plantSystem.plants[i].group === hitObj.parent) {
        hitPlant = this.plantSystem.plants[i];
        break;
      }
    }
    if (!hitPlant) return;

    // Boost growth
    hitPlant.growthRate *= 3;
    hitPlant.targetHeight *= 1.3;

    // Boost glow
    if (hitPlant.emissiveMat) {
      hitPlant.emissiveMat.emissiveIntensity = Math.min(
        hitPlant.emissiveMat.emissiveIntensity + 0.5,
        2.0
      );
    }

    // Particle burst from the plant
    var burstPos = hitPlant.position.clone();
    burstPos.y += hitPlant.targetHeight * 0.5;
    this.particleSystem.emit(burstPos, 30, {
      spread: 0.3,
      speed: 2.0,
      life: 2.0,
      size: 0.08,
      color: new THREE.Color(0xffaa33)
    });
  };

  App.prototype.updateCursorWorld = function () {
    // Project mouse into world space at a reasonable depth
    this._mouseVec2.set(this.mouse.x, this.mouse.y);
    this.raycaster.setFromCamera(this._mouseVec2, this.camera);
    this.raycaster.ray.intersectPlane(this._rayPlane, this._rayTarget);
    if (this._rayTarget) {
      this.cursorWorld.copy(this._rayTarget);
    }
  };

  App.prototype.start = function () {
    var self = this;
    (function loop() {
      requestAnimationFrame(loop);
      var delta = self.clock.getDelta();
      var time = self.clock.getElapsedTime();
      self.controls.update();

      // Update cursor world position
      self.updateCursorWorld();

      // Update weather (wind, rain, lighting)
      if (self.weather) {
        self.weather.update(delta, time);
      }

      // Update plants with wind
      if (self.plantSystem) {
        self.plantSystem.update(delta, time);
      }

      // Update particles with wind
      if (self.particleSystem) {
        var wind = self.weather ? self.weather.getWind() : null;
        self.particleSystem.update(delta, time, wind);

        // Spawn ambient pollen
        if (Math.random() < 0.1) {
          self._pollenPos.set(
            (Math.random() - 0.5) * 8,
            1 + Math.random() * 3,
            (Math.random() - 0.5) * 10
          );
          self.particleSystem.emit(self._pollenPos, 1, {
            spread: 0.2,
            speed: 0.3,
            life: 5,
            size: 0.04,
            color: self._pollenColor
          });
        }
      }

      // Update fireflies
      if (self.fireflyManager) {
        self.fireflyManager.update(delta, time, self.cursorWorld);
      }

      // Adjust plant glow based on time of night
      if (self.plantSystem) {
        var nightFactor = self.timeOfNight;
        for (var i = 0; i < self.plantSystem.plants.length; i++) {
          var plant = self.plantSystem.plants[i];
          if (plant.emissiveMat) {
            var baseGlow = plant.species.emissiveIntensity;
            var nightBoost = nightFactor * plant.species.glowIntensity;
            plant.emissiveMat.emissiveIntensity = baseGlow + nightBoost;
          }
        }
      }

      self.renderer.render(self.scene, self.camera);
      if (++self.frames === 2) {
        var boot = document.getElementById('boot');
        if (boot) boot.remove();
        window.__greenhouse = { ready: true, stage: 'greenhouse', frames: self.frames };
      }
    })();
  };

  /* ------------------------------------------------------------------ */
  /*  Bootstrap                                                          */
  /* ------------------------------------------------------------------ */

  var app = new App();
  app.init();
  app.start();

})();
