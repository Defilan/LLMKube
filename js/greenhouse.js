// Greenhouse: the structure itself (floor, metal framework, glass, interior).
// Extracted from the single inline script so each file stays small enough to
// rewrite in one tool call; a whole-file rewrite of the monolith overran the
// model's output budget and truncated the tool-call JSON mid-write.
//
// Attached to window so the other files can use it without a module system,
// which file:// forbids.
(function () {
  'use strict';

  var Greenhouse = function (scene) {
    this.scene = scene;
  };

  Greenhouse.prototype.build = function () {
    this.buildFloor();
    this.buildFramework();
    this.buildGlass();
    this.buildInterior();
  };

  /* -- Floor -- */
  Greenhouse.prototype.buildFloor = function () {
    var geo = new THREE.PlaneGeometry(12, 16);
    var mat = new THREE.MeshStandardMaterial({
      color: 0x1a2a2a,
      roughness: 0.85,
      metalness: 0.05
    });
    var floor = new THREE.Mesh(geo, mat);
    floor.rotation.x = -Math.PI / 2;
    floor.position.y = 0;
    this.scene.add(floor);
  };

  /* -- Metal framework (posts, beams, glazing bars) -- */
  Greenhouse.prototype.buildFramework = function () {
    var mat = new THREE.MeshStandardMaterial({
      color: 0x2a3a3a,
      roughness: 0.3,
      metalness: 0.8
    });

    var W = 6, D = 8, H = 4, roofH = 2;
    var halfW = W / 2, halfD = D / 2;

    // Vertical posts at corners and midpoints
    var postPositions = [
      [-halfW, 0, -halfD], [halfW, 0, -halfD],
      [-halfW, 0, halfD],  [halfW, 0, halfD],
      [-halfW, 0, 0],     [halfW, 0, 0],
      [0, 0, -halfD],     [0, 0, halfD]
    ];

    var postGeo = new THREE.BoxGeometry(0.08, H, 0.08);
    postPositions.forEach(function (p) {
      var post = new THREE.Mesh(postGeo, mat);
      post.position.set(p[0], H / 2, p[2]);
      this.scene.add(post);
    }, this);

    // Ridge posts (taller, at the roof peak)
    var ridgePostGeo = new THREE.BoxGeometry(0.08, roofH, 0.08);
    var ridgePositions = [
      [-halfW, H, 0], [halfW, H, 0],
      [0, H, -halfD], [0, H, halfD]
    ];
    ridgePositions.forEach(function (p) {
      var post = new THREE.Mesh(ridgePostGeo, mat);
      post.position.set(p[0], H + roofH / 2, p[2]);
      this.scene.add(post);
    }, this);

    // Horizontal beams along walls (top of walls)
    var beamGeoW = new THREE.BoxGeometry(W, 0.06, 0.06);
    var beamGeoD = new THREE.BoxGeometry(0.06, 0.06, D);

    // Top beams on long walls
    var b1 = new THREE.Mesh(beamGeoW, mat);
    b1.position.set(0, H, -halfD);
    this.scene.add(b1);
    var b2 = new THREE.Mesh(beamGeoW, mat);
    b2.position.set(0, H, halfD);
    this.scene.add(b2);

    // Top beams on short walls
    var b3 = new THREE.Mesh(beamGeoD, mat);
    b3.position.set(-halfW, H, 0);
    this.scene.add(b3);
    var b4 = new THREE.Mesh(beamGeoD, mat);
    b4.position.set(halfW, H, 0);
    this.scene.add(b4);

    // Mid-height beams
    var b5 = new THREE.Mesh(beamGeoW, mat);
    b5.position.set(0, H * 0.5, -halfD);
    this.scene.add(b5);
    var b6 = new THREE.Mesh(beamGeoW, mat);
    b6.position.set(0, H * 0.5, halfD);
    this.scene.add(b6);

    // Ridge beam
    var ridgeGeo = new THREE.BoxGeometry(W, 0.06, 0.06);
    var ridge = new THREE.Mesh(ridgeGeo, mat);
    ridge.position.set(0, H + roofH, 0);
    this.scene.add(ridge);

    // Roof slope beams (from wall top to ridge)
    var slopeLen = Math.sqrt(halfD * halfD + roofH * roofH);
    var slopeAngle = Math.atan2(roofH, halfD);
    var slopeGeo = new THREE.BoxGeometry(0.06, slopeLen, 0.06);

    var slopePositions = [
      { x: -halfW, z: 0, side: -1 },
      { x: halfW, z: 0, side: -1 },
      { x: -halfW, z: 0, side: 1 },
      { x: halfW, z: 0, side: 1 },
      { x: 0, z: 0, side: -1 },
      { x: 0, z: 0, side: 1 }
    ];

    slopePositions.forEach(function (s) {
      var beam = new THREE.Mesh(slopeGeo, mat);
      var midZ = s.side * halfD / 2;
      var midY = H + roofH / 2;
      beam.position.set(s.x, midY, midZ);
      beam.rotation.x = -s.side * slopeAngle;
      this.scene.add(beam);
    }, this);

    // Glazing bars (horizontal dividers on walls)
    var barGeoW = new THREE.BoxGeometry(W, 0.04, 0.04);
    var barGeoD = new THREE.BoxGeometry(0.04, 0.04, D);

    [H * 0.25, H * 0.75].forEach(function (y) {
      var bar1 = new THREE.Mesh(barGeoW, mat);
      bar1.position.set(0, y, -halfD);
      this.scene.add(bar1);
      var bar2 = new THREE.Mesh(barGeoW, mat);
      bar2.position.set(0, y, halfD);
      this.scene.add(bar2);
    }, this);

    [-halfW, halfW].forEach(function (x) {
      [D / 3, -D / 3].forEach(function (z) {
        var bar = new THREE.Mesh(barGeoD, mat);
        bar.position.set(x, H * 0.5, z);
        this.scene.add(bar);
      }, this);
    }, this);
  };

  /* -- Glass walls and roof -- */
  Greenhouse.prototype.buildGlass = function () {
    var glassMat = new THREE.MeshPhysicalMaterial({
      color: 0x88ccdd,
      transmission: 0.92,
      opacity: 1,
      metalness: 0,
      roughness: 0.05,
      ior: 1.5,
      thickness: 0.01,
      transparent: true,
      side: THREE.DoubleSide
    });

    var W = 6, D = 8, H = 4, roofH = 2;
    var halfW = W / 2, halfD = D / 2;

    // Side walls (long walls)
    var wallGeo = new THREE.PlaneGeometry(W, H);
    var wall1 = new THREE.Mesh(wallGeo, glassMat);
    wall1.position.set(0, H / 2, -halfD);
    this.scene.add(wall1);
    var wall2 = new THREE.Mesh(wallGeo, glassMat);
    wall2.position.set(0, H / 2, halfD);
    wall2.rotation.y = Math.PI;
    this.scene.add(wall2);

    // End walls (short walls)
    var endGeo = new THREE.PlaneGeometry(D, H);
    var end1 = new THREE.Mesh(endGeo, glassMat);
    end1.position.set(-halfW, H / 2, 0);
    end1.rotation.y = Math.PI / 2;
    this.scene.add(end1);
    var end2 = new THREE.Mesh(endGeo, glassMat);
    end2.position.set(halfW, H / 2, 0);
    end2.rotation.y = -Math.PI / 2;
    this.scene.add(end2);

    // Roof panels (two sloped planes)
    var slopeLen = Math.sqrt(halfD * halfD + roofH * roofH);
    var roofGeo = new THREE.PlaneGeometry(W, slopeLen);

    var roof1 = new THREE.Mesh(roofGeo, glassMat);
    roof1.position.set(0, H + roofH / 2, -halfD / 2);
    roof1.rotation.x = Math.atan2(roofH, halfD);
    this.scene.add(roof1);

    var roof2 = new THREE.Mesh(roofGeo, glassMat);
    roof2.position.set(0, H + roofH / 2, halfD / 2);
    roof2.rotation.x = -Math.atan2(roofH, halfD);
    this.scene.add(roof2);
  };

  /* -- Interior: benches, planters, pots -- */
  Greenhouse.prototype.buildInterior = function () {
    var woodMat = new THREE.MeshStandardMaterial({
      color: 0x3a2a1a,
      roughness: 0.7,
      metalness: 0.05
    });
    var potMat = new THREE.MeshStandardMaterial({
      color: 0x5a3a2a,
      roughness: 0.6,
      metalness: 0.05
    });

    // Center bench
    var benchTop = new THREE.Mesh(
      new THREE.BoxGeometry(4, 0.08, 0.8),
      woodMat
    );
    benchTop.position.set(0, 0.9, 0);
    this.scene.add(benchTop);

    // Bench legs
    var legGeo = new THREE.BoxGeometry(0.08, 0.9, 0.08);
    [[-1.8, 0.45, -0.3], [1.8, 0.45, -0.3],
     [-1.8, 0.45, 0.3], [1.8, 0.45, 0.3]].forEach(function (p) {
      var leg = new THREE.Mesh(legGeo, woodMat);
      leg.position.set(p[0], p[1], p[2]);
      this.scene.add(leg);
    }, this);

    // Side benches
    [-2.2, 2.2].forEach(function (x) {
      var top = new THREE.Mesh(
        new THREE.BoxGeometry(0.6, 0.08, 5),
        woodMat
      );
      top.position.set(x, 0.7, 0);
      this.scene.add(top);

      [[x - 0.2, 0.35, -2], [x + 0.2, 0.35, -2],
       [x - 0.2, 0.35, 2], [x + 0.2, 0.35, 2]].forEach(function (p) {
        var leg = new THREE.Mesh(legGeo, woodMat);
        leg.position.set(p[0], p[1], p[2]);
        this.scene.add(leg);
      }, this);
    }, this);

    // Planters along the back wall
    var planterGeo = new THREE.BoxGeometry(2, 0.5, 0.5);
    [-1.5, 1.5].forEach(function (x) {
      var planter = new THREE.Mesh(planterGeo, woodMat);
      planter.position.set(x, 0.25, -3.5);
      this.scene.add(planter);
    }, this);

    // Pots on the center bench
    var potGeo = new THREE.CylinderGeometry(0.15, 0.12, 0.25, 8);
    [-1.2, -0.4, 0.4, 1.2].forEach(function (x) {
      var pot = new THREE.Mesh(potGeo, potMat);
      pot.position.set(x, 1.05, 0);
      this.scene.add(pot);
    }, this);

    // Pots on side benches
    [-2.2, 2.2].forEach(function (x) {
      [-1.5, -0.5, 0.5, 1.5].forEach(function (z) {
        var pot = new THREE.Mesh(potGeo, potMat);
        pot.position.set(x, 0.85, z);
        this.scene.add(pot);
      }, this);
    }, this);

    // Large planter pots on the floor
    var bigPotGeo = new THREE.CylinderGeometry(0.3, 0.25, 0.5, 8);
    [[-2.5, 0.25, -3], [2.5, 0.25, -3],
     [-2.5, 0.25, 3], [2.5, 0.25, 3]].forEach(function (p) {
      var pot = new THREE.Mesh(bigPotGeo, potMat);
      pot.position.set(p[0], p[1], p[2]);
      this.scene.add(pot);
    }, this);
  };

  /* ------------------------------------------------------------------ */
  /*  App                                                                */
  /* ------------------------------------------------------------------ */

  window.Greenhouse = Greenhouse;
})();
