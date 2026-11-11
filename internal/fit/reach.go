package fit

import "math"

// Dogma attributes the reach model reads.
const (
	attrOptimalSigRadius         = 620
	attrAoeVelocity              = 653
	attrAoeCloudSize             = 654
	attrAoeDamageReduction       = 1353
	attrExplosionDelay           = 281 // missile flight time, ms
	attrEmpFieldRange            = 99
	attrDroneControlDistance     = 458
	attrFighterAttackReduction   = 2231
	attrFighterAttackSensitivity = 2232
	attrFighterAttackExpRadius   = 2234
	attrFighterAttackExpVel      = 2235
)

// Reach kinds: how a weapon group's damage depends on distance and
// on the target.
const (
	// ReachTurret: hit chance falls off past optimal over falloff,
	// and with the target's angular speed against tracking and
	// signature. Entropic disintegrators have no falloff.
	ReachTurret = "turret"
	// ReachSentry: a sentry drone, a turret sitting by the ship,
	// within drone control range.
	ReachSentry = "sentry"
	// ReachMissile: full damage out to the missile's flight range,
	// applied by explosion radius and velocity.
	ReachMissile = "missile"
	// ReachArea: Vorton projectors, full damage out to optimal,
	// applied by explosion radius and velocity like a missile.
	ReachArea = "area"
	// ReachSmartbomb: full damage inside the blast radius.
	ReachSmartbomb = "smartbomb"
	// ReachDrone: drones fly to the target within control range
	// and fight from their own optimal range.
	ReachDrone = "drone"
	// ReachFighter: fighters fly to the target wherever it is and
	// apply their attack like a missile.
	ReachFighter = "fighter"
)

// WeaponReach is one fitted weapon group's sustained damage and what
// its reach and application depend on. Distances are metres.
type WeaponReach struct {
	TypeID int64
	Kind   string
	Count  int
	DPS    float64 // the group's paper DPS

	Optimal       float64
	Falloff       float64
	Tracking      float64
	SigResolution float64

	// Range is the hard reach: missile flight range, smartbomb
	// radius, drone control range.
	Range float64
	// Speed is a drone's own top speed: it cannot catch a faster
	// target.
	Speed float64

	ExplosionRadius   float64
	ExplosionVelocity float64
	// DamageReduction is the explosion's application exponent.
	DamageReduction float64
}

// Target is what the weapons shoot at: its signature radius (m) and
// its speed (m/s), taken as moving straight across the shooter's
// line of fire.
type Target struct {
	Signature float64
	Velocity  float64
}

// DamageAt is the group's expected damage per second at distance d
// metres from the ship, against t (nil: a target every weapon fully
// hits, so only reach counts). Turret figures include wrecking hits.
func (w WeaponReach) DamageAt(d float64, t *Target) float64 {
	switch w.Kind {
	case ReachTurret:
		return w.DPS * turretDamageMult(turretHitChance(d, d, w, t))
	case ReachSentry:
		if d > w.Range {
			return 0
		}
		return w.DPS * turretDamageMult(turretHitChance(d, d, w, t))
	case ReachMissile:
		if d > w.Range {
			return 0
		}
		return w.DPS * explosionApplication(w, t)
	case ReachArea:
		if d > w.Optimal {
			return 0
		}
		return w.DPS * explosionApplication(w, t)
	case ReachSmartbomb:
		if d > w.Range {
			return 0
		}
		return w.DPS
	case ReachDrone:
		if d > w.Range {
			return 0
		}
		if t != nil && t.Velocity > w.Speed {
			return 0 // the drones never catch it
		}
		// The drones fight from their own optimal range,
		// wherever the target sits.
		at := math.Max(w.Optimal, 1)
		return w.DPS * turretDamageMult(turretHitChance(at, at, w, t))
	case ReachFighter:
		return w.DPS * explosionApplication(w, t)
	}
	return 0
}

// turretHitChance is the game's chance to hit: 0.5 to the power of
// the tracking term plus the range term. rangeD sets the range term,
// angularD the distance the target's speed turns into angular speed.
func turretHitChance(rangeD, angularD float64, w WeaponReach, t *Target) float64 {
	rangeTerm := 0.0
	if rangeD > w.Optimal {
		if w.Falloff <= 0 {
			return 0
		}
		x := (rangeD - w.Optimal) / w.Falloff
		rangeTerm = x * x
	}
	trackTerm := 0.0
	if t != nil && t.Velocity > 0 {
		if w.Tracking <= 0 || t.Signature <= 0 {
			return 0
		}
		angular := t.Velocity / math.Max(angularD, 1)
		x := angular * w.SigResolution / (w.Tracking * t.Signature)
		trackTerm = x * x
	}
	return math.Pow(0.5, rangeTerm+trackTerm)
}

// turretDamageMult turns a hit chance into expected damage per shot,
// as a share of the paper figure: the first 1% of hits are wrecking
// shots at triple damage; other hits land between 0.5x and 1.49x of
// the paper figure, better the likelier the hit.
func turretDamageMult(chance float64) float64 {
	if chance <= 0 {
		return 0
	}
	wreck := math.Min(chance, 0.01)
	mult := wreck * 3
	if normal := chance - wreck; normal > 0 {
		mult += normal * ((0.01+chance)/2 + 0.49)
	}
	return mult
}

// explosionApplication is the share of a missile's (or Vorton's, or
// fighter attack's) damage a target takes: limited by its signature
// against the explosion radius, and by its speed against the
// explosion velocity raised to the damage reduction exponent.
func explosionApplication(w WeaponReach, t *Target) float64 {
	if t == nil || w.ExplosionRadius <= 0 {
		return 1
	}
	sig := t.Signature / w.ExplosionRadius
	app := math.Min(1, sig)
	if t.Velocity > 0 && w.ExplosionVelocity > 0 && w.DamageReduction > 0 {
		app = math.Min(app, math.Pow(sig*w.ExplosionVelocity/t.Velocity, w.DamageReduction))
	}
	return app
}

// ReachEnd is the distance past which the group does no damage
// (turrets: optimal plus three falloffs, where under 0.2% of hits
// land); 0 when its reach does not end (fighters).
func (w WeaponReach) ReachEnd() float64 {
	switch w.Kind {
	case ReachTurret:
		return w.Optimal + 3*w.Falloff
	case ReachSentry:
		return math.Min(w.Optimal+3*w.Falloff, w.Range)
	case ReachArea:
		return w.Optimal
	case ReachMissile, ReachSmartbomb, ReachDrone:
		return w.Range
	}
	return 0
}
