// App: renderer, scene, camera, controls, lighting, and the render loop.
// Owns bootstrap. Later stages attach their systems here rather than editing
// Greenhouse.
(function () {
  'use strict';

  var Greenhouse = window.Greenhouse;

  var App = function () {
    this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    this.renderer.setSize(window.innerWidth, window.innerHeight);
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.0;
    document.body.appendChild(this.renderer.domElement);

    this.scene = new THREE.Scene();
    this.scene.fog = new THREE.FogExp2(0x05080c, 0.04);

    this.camera = new THREE.PerspectiveCamera(
      55, window.innerWidth / window.innerHeight, 0.1, 200
    );
    this.camera.position.set(0, 2.5, 7);

    this.clock = new THREE.Clock();
    this.frames = 0;
  };

  App.prototype.init = function () {
    this.buildLighting();
    this.buildGreenhouse();
    this.setupControls();
    this.setupResize();
  };

  App.prototype.buildLighting = function () {
    // Ambient: deep teal fill
    var ambient = new THREE.AmbientLight(0x0a2a3a, 0.4);
    this.scene.add(ambient);

    // Moonlight: cool directional from above
    var moon = new THREE.DirectionalLight(0x6688aa, 0.8);
    moon.position.set(3, 10, -5);
    this.scene.add(moon);

    // Warm interior pool lights
    var warm1 = new THREE.PointLight(0xffaa44, 8, 12, 2);
    warm1.position.set(0, 3.5, 0);
    this.scene.add(warm1);

    var warm2 = new THREE.PointLight(0xff8833, 5, 10, 2);
    warm2.position.set(-2, 3, -3);
    this.scene.add(warm2);

    var warm3 = new THREE.PointLight(0xff8833, 5, 10, 2);
    warm3.position.set(2, 3, 3);
    this.scene.add(warm3);

    // Subtle magenta accent
    var magenta = new THREE.PointLight(0xcc44aa, 3, 8, 2);
    magenta.position.set(0, 1.5, -3.5);
    this.scene.add(magenta);

    // Amber accent near floor
    var amber = new THREE.PointLight(0xffcc44, 4, 6, 2);
    amber.position.set(0, 0.5, 0);
    this.scene.add(amber);
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

  App.prototype.start = function () {
    var self = this;
    (function loop() {
      requestAnimationFrame(loop);
      var delta = self.clock.getDelta();
      self.controls.update();
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
