// Plants: procedural plant generation, growth, and wind sway.
// Plant class = one individual. PlantSystem = collection + species.
// Attached to window so other files can use it without a module system.
(function () {
  'use strict';

  /* ---- colour palette ---- */
  var PALETTE = {
    teal:      0x0a6e6e,
    indigo:    0x2a1a5e,
    magenta:   0xcc44aa,
    amber:     0xffaa33,
    deepTeal:  0x064a4a,
    softGreen: 0x1a5a3a,
    darkGreen: 0x0a3a2a,
    warmAmber: 0xffcc44,
    softMagenta: 0xaa3388
  };

  /* ---- species definitions ---- */
  // Each species describes how to build a plant procedurally.
  var SPECIES = [
    {
      name: 'fern',
      stemColor: PALETTE.deepTeal,
      leafColor: PALETTE.teal,
      emissiveColor: PALETTE.amber,
      emissiveIntensity: 0.15,
      maxHeight: 1.2,
      minHeight: 0.5,
      leafCount: 12,
      leafShape: 'frond',
      branching: 3,
      stemThickness: 0.015,
      glowIntensity: 0.3
    },
    {
      name: 'vine',
      stemColor: PALETTE.darkGreen,
      leafColor: PALETTE.softGreen,
      emissiveColor: PALETTE.warmAmber,
      emissiveIntensity: 0.1,
      maxHeight: 2.0,
      minHeight: 0.8,
      leafCount: 8,
      leafShape: 'oval',
      branching: 2,
      stemThickness: 0.01,
      glowIntensity: 0.2,
      trailing: true
    },
    {
      name: 'succulent',
      stemColor: PALETTE.deepTeal,
      leafColor: PALETTE.teal,
      emissiveColor: PALETTE.amber,
      emissiveIntensity: 0.2,
      maxHeight: 0.5,
      minHeight: 0.2,
      leafCount: 16,
      leafShape: 'spike',
      branching: 1,
      stemThickness: 0.02,
      glowIntensity: 0.4
    },
    {
      name: 'flowering',
      stemColor: PALETTE.indigo,
      leafColor: PALETTE.teal,
      emissiveColor: PALETTE.magenta,
      emissiveIntensity: 0.25,
      maxHeight: 1.8,
      minHeight: 0.8,
      leafCount: 6,
      leafShape: 'broad',
      branching: 2,
      stemThickness: 0.018,
      glowIntensity: 0.5,
      hasFlower: true
    },
    {
      name: 'broadleaf',
      stemColor: PALETTE.darkGreen,
      leafColor: PALETTE.softGreen,
      emissiveColor: PALETTE.warmAmber,
      emissiveIntensity: 0.12,
      maxHeight: 1.5,
      minHeight: 0.6,
      leafCount: 8,
      leafShape: 'broad',
      branching: 2,
      stemThickness: 0.025,
      glowIntensity: 0.25
    },
    {
      name: 'spiky',
      stemColor: PALETTE.indigo,
      leafColor: PALETTE.indigo,
      emissiveColor: PALETTE.magenta,
      emissiveIntensity: 0.3,
      maxHeight: 0.8,
      minHeight: 0.3,
      leafCount: 20,
      leafShape: 'spike',
      branching: 1,
      stemThickness: 0.012,
      glowIntensity: 0.5
    },
    {
      name: 'tallstem',
      stemColor: PALETTE.deepTeal,
      leafColor: PALETTE.teal,
      emissiveColor: PALETTE.amber,
      emissiveIntensity: 0.18,
      maxHeight: 2.5,
      minHeight: 1.2,
      leafCount: 4,
      leafShape: 'narrow',
      branching: 1,
      stemThickness: 0.02,
      glowIntensity: 0.35
    },
    {
      name: 'bush',
      stemColor: PALETTE.darkGreen,
      leafColor: PALETTE.softGreen,
      emissiveColor: PALETTE.warmAmber,
      emissiveIntensity: 0.15,
      maxHeight: 0.9,
      minHeight: 0.4,
      leafCount: 24,
      leafShape: 'oval',
      branching: 4,
      stemThickness: 0.015,
      glowIntensity: 0.3
    },
    {
      name: 'moss',
      stemColor: PALETTE.deepTeal,
      leafColor: PALETTE.teal,
      emissiveColor: PALETTE.amber,
      emissiveIntensity: 0.2,
      maxHeight: 0.3,
      minHeight: 0.1,
      leafCount: 30,
      leafShape: 'oval',
      branching: 1,
      stemThickness: 0.005,
      glowIntensity: 0.4
    },
    {
      name: 'cactus',
      stemColor: PALETTE.indigo,
      leafColor: PALETTE.indigo,
      emissiveColor: PALETTE.magenta,
      emissiveIntensity: 0.22,
      maxHeight: 1.0,
      minHeight: 0.4,
      leafCount: 12,
      leafShape: 'spike',
      branching: 2,
      stemThickness: 0.03,
      glowIntensity: 0.45
    }
  ];

  /* ---- shared geometries (created once, reused) ---- */
  var SHARED = {};

  function initSharedGeometries() {
    // Stem segments
    SHARED.stemCyl = new THREE.CylinderGeometry(1, 1, 1, 6);
    // Leaf shapes
    SHARED.leafFrond = new THREE.PlaneGeometry(1, 0.3, 4, 1);
    SHARED.leafOval = new THREE.PlaneGeometry(0.4, 0.6, 3, 2);
    SHARED.leafSpike = new THREE.ConeGeometry(0.05, 0.4, 4);
    SHARED.leafBroad = new THREE.PlaneGeometry(0.6, 0.8, 3, 2);
    SHARED.leafNarrow = new THREE.PlaneGeometry(0.15, 0.8, 2, 1);
    // Flower
    SHARED.flower = new THREE.SphereGeometry(0.08, 6, 4);
  }

  /* ---- Plant class ---- */
  var Plant = function (species, position, scene) {
    this.species = species;
    this.position = position.clone();
    this.scene = scene;

    // Randomised per-plant properties
    this.targetHeight = species.minHeight +
      Math.random() * (species.maxHeight - species.minHeight);
    this.currentHeight = 0;
    this.growthRate = 0.02 + Math.random() * 0.03;

    // Wind sway
    this.swayPhase = Math.random() * Math.PI * 2;
    this.swaySpeed = 0.5 + Math.random() * 0.5;
    this.swayAmplitude = 0.02 + Math.random() * 0.04;

    // Emissive glow
    this.glowPhase = Math.random() * Math.PI * 2;
    this.glowSpeed = 0.3 + Math.random() * 0.4;

    // Build the plant
    this.group = new THREE.Group();
    this.group.position.copy(this.position);
    this.scene.add(this.group);

    this.segments = [];   // stem segments (for sway)
    this.leaves = [];     // leaf meshes
    this.flower = null;
    this.emissiveMat = null;

    this.build();
  };

  Plant.prototype.build = function () {
    var sp = this.species;
    var stemMat = new THREE.MeshStandardMaterial({
      color: sp.stemColor,
      roughness: 0.7,
      metalness: 0.1
    });

    // Build stem segments
    var segCount = Math.max(3, Math.floor(this.targetHeight * 4));
    var segHeight = this.targetHeight / segCount;
    var prevPos = new THREE.Vector3(0, 0, 0);

    for (var i = 0; i < segCount; i++) {
      var t = i / segCount;
      var thickness = sp.stemThickness * (1 - t * 0.5);
      var geo = SHARED.stemCyl.clone();
      geo.scale(thickness, segHeight, thickness);

      var seg = new THREE.Mesh(geo, stemMat);
      seg.position.copy(prevPos);
      seg.position.y += segHeight / 2;
      this.group.add(seg);

      this.segments.push({
        mesh: seg,
        basePos: prevPos.clone(),
        baseY: segHeight / 2,
        t: t,
        segHeight: segHeight
      });

      prevPos.y += segHeight;
    }

    // Build leaves
    this.buildLeaves(stemMat);

    // Build flower if applicable
    if (sp.hasFlower) {
      this.buildFlower();
    }
  };

  Plant.prototype.buildLeaves = function () {
    var sp = this.species;
    var leafGeo;

    switch (sp.leafShape) {
      case 'frond': leafGeo = SHARED.leafFrond; break;
      case 'oval':  leafGeo = SHARED.leafOval; break;
      case 'spike': leafGeo = SHARED.leafSpike; break;
      case 'broad': leafGeo = SHARED.leafBroad; break;
      case 'narrow': leafGeo = SHARED.leafNarrow; break;
      default:      leafGeo = SHARED.leafOval; break;
    }

    // Emissive material for leaves
    this.emissiveMat = new THREE.MeshStandardMaterial({
      color: sp.leafColor,
      emissive: sp.emissiveColor,
      emissiveIntensity: sp.emissiveIntensity,
      roughness: 0.6,
      metalness: 0.05,
      side: THREE.DoubleSide
    });

    var leafCount = sp.leafCount;
    for (var i = 0; i < leafCount; i++) {
      var t = (i + 1) / (leafCount + 1); // distribute along stem
      var leaf = new THREE.Mesh(leafGeo, this.emissiveMat);

      // Position along stem
      var attachY = t * this.targetHeight;
      var angle = (i / leafCount) * Math.PI * 2 * sp.branching;
      var radius = 0.05 + Math.random() * 0.15;

      leaf.position.set(
        Math.cos(angle) * radius,
        attachY,
        Math.sin(angle) * radius
      );

      // Orient leaf
      leaf.rotation.y = -angle;
      leaf.rotation.x = -0.3 + Math.random() * 0.6;
      leaf.rotation.z = (Math.random() - 0.5) * 0.4;

      // Scale variation
      var s = 0.5 + Math.random() * 0.8;
      leaf.scale.set(s, s, s);

      this.group.add(leaf);
      this.leaves.push({
        mesh: leaf,
        basePos: leaf.position.clone(),
        baseRot: leaf.rotation.clone(),
        t: t,
        angle: angle,
        radius: radius
      });
    }
  };

  Plant.prototype.buildFlower = function () {
    var flowerMat = new THREE.MeshStandardMaterial({
      color: PALETTE.magenta,
      emissive: PALETTE.magenta,
      emissiveIntensity: 0.6,
      roughness: 0.3,
      metalness: 0.1
    });

    this.flower = new THREE.Mesh(SHARED.flower, flowerMat);
    this.flower.position.y = this.targetHeight + 0.05;
    this.group.add(this.flower);
  };

  Plant.prototype.update = function (delta, time) {
    // Growth
    if (this.currentHeight < this.targetHeight) {
      this.currentHeight = Math.min(
        this.currentHeight + this.growthRate * delta,
        this.targetHeight
      );
    }

    var growthRatio = this.currentHeight / this.targetHeight;

    // Update stem segments
    for (var i = 0; i < this.segments.length; i++) {
      var seg = this.segments[i];
      var swayX = Math.sin(time * this.swaySpeed + this.swayPhase + seg.t * 3)
        * this.swayAmplitude * seg.t;
      var swayZ = Math.cos(time * this.swaySpeed * 0.7 + this.swayPhase + seg.t * 2)
        * this.swayAmplitude * seg.t * 0.5;

      seg.mesh.position.x = swayX;
      seg.mesh.position.y = seg.baseY * growthRatio;
      seg.mesh.position.z = swayZ;

      // Scale Y by growth
      seg.mesh.scale.y = growthRatio;
    }

    // Update leaves
    for (var j = 0; j < this.leaves.length; j++) {
      var leaf = this.leaves[j];
      var swayX = Math.sin(time * this.swaySpeed + this.swayPhase + leaf.t * 4)
        * this.swayAmplitude * leaf.t;
      var swayZ = Math.cos(time * this.swaySpeed * 0.7 + this.swayPhase + leaf.t * 3)
        * this.swayAmplitude * leaf.t * 0.5;

      leaf.mesh.position.x = leaf.basePos.x + swayX;
      leaf.mesh.position.y = leaf.basePos.y * growthRatio;
      leaf.mesh.position.z = leaf.basePos.z + swayZ;

      // Gentle leaf rotation sway
      leaf.mesh.rotation.x = leaf.baseRot.x +
        Math.sin(time * this.swaySpeed * 1.2 + this.swayPhase) * 0.1;
      leaf.mesh.rotation.z = leaf.baseRot.z +
        Math.cos(time * this.swaySpeed * 0.9 + this.swayPhase) * 0.08;

      // Scale by growth
      leaf.mesh.scale.setScalar(growthRatio);
    }

    // Update flower
    if (this.flower) {
      this.flower.position.y = this.targetHeight * growthRatio + 0.05;
      this.flower.scale.setScalar(growthRatio);
    }

    // Emissive glow pulsing
    if (this.emissiveMat) {
      var glow = sp.emissiveIntensity +
        Math.sin(time * this.glowSpeed + this.glowPhase) * sp.glowIntensity * 0.3;
      this.emissiveMat.emissiveIntensity = Math.max(0, glow);
    }
  };

  /* ---- PlantSystem class ---- */
  var PlantSystem = function (scene) {
    this.scene = scene;
    this.plants = [];
  };

  PlantSystem.prototype.init = function () {
    initSharedGeometries();
    this.seedPlants();
  };

  PlantSystem.prototype.seedPlants = function () {
    var positions = this.getPlantPositions();

    for (var i = 0; i < positions.length; i++) {
      var pos = positions[i];
      var species = SPECIES[pos.speciesIndex % SPECIES.length];
      var plant = new Plant(species, pos.position, this.scene);
      this.plants.push(plant);
    }
  };

  PlantSystem.prototype.getPlantPositions = function () {
    var positions = [];

    // Center bench pots (y=1.05 + 0.125 = 1.175)
    var benchPositions = [-1.2, -0.4, 0.4, 1.2];
    for (var i = 0; i < benchPositions.length; i++) {
      positions.push({
        position: new THREE.Vector3(benchPositions[i], 1.175, 0),
        speciesIndex: i
      });
    }

    // Side bench pots (y=0.85 + 0.125 = 0.975)
    var sideX = [-2.2, 2.2];
    var sideZ = [-1.5, -0.5, 0.5, 1.5];
    var idx = 0;
    for (var sx = 0; sx < sideX.length; sx++) {
      for (var sz = 0; sz < sideZ.length; sz++) {
        positions.push({
          position: new THREE.Vector3(sideX[sx], 0.975, sideZ[sz]),
          speciesIndex: idx
        });
        idx++;
      }
    }

    // Back wall planters (y=0.25 + 0.25 = 0.5)
    var planterX = [-1.5, 1.5];
    for (var px = 0; px < planterX.length; px++) {
      positions.push({
        position: new THREE.Vector3(planterX[px], 0.5, -3.5),
        speciesIndex: idx
      });
      idx++;
    }

    // Floor big pots (y=0.25 + 0.25 = 0.5)
    var floorPositions = [
      [-2.5, -3], [2.5, -3],
      [-2.5, 3], [2.5, 3]
    ];
    for (var fp = 0; fp < floorPositions.length; fp++) {
      positions.push({
        position: new THREE.Vector3(
          floorPositions[fp][0], 0.5, floorPositions[fp][1]
        ),
        speciesIndex: idx
      });
      idx++;
    }

    // Extra floor plants in beds (scattered)
    var extraPositions = [
      [-3, 0, -2], [-3, 0, 0], [-3, 0, 2],
      [3, 0, -2], [3, 0, 0], [3, 0, 2],
      [-1, 0, -3.5], [1, 0, -3.5],
      [-1, 0, 3.5], [1, 0, 3.5],
      [0, 0, -3.5], [0, 0, 3.5],
      [-2, 0, -1], [2, 0, -1],
      [-2, 0, 1], [2, 0, 1]
    ];
    for (var ep = 0; ep < extraPositions.length; ep++) {
      positions.push({
        position: new THREE.Vector3(
          extraPositions[ep][0], 0, extraPositions[ep][1]
        ),
        speciesIndex: idx
      });
      idx++;
    }

    return positions;
  };

  PlantSystem.prototype.update = function (delta, time) {
    for (var i = 0; i < this.plants.length; i++) {
      this.plants[i].update(delta, time);
    }
  };

  /* ---- expose ---- */
  window.PlantSystem = PlantSystem;
})();
