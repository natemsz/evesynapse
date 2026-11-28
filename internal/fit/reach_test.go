package fit

import "testing"

// TestWeaponReachDamageAt pins the range and application math the
// damage-at-range chart draws: turret hit chance over falloff with
// wrecking hits averaged in, missile explosion application, and a
// Breacher pod's cap or share of the target's hit points.
func TestWeaponReachDamageAt(t *testing.T) {
	gun := WeaponReach{Kind: ReachTurret, DPS: 100, Optimal: 3600, Falloff: 5000, Tracking: 38.4, SigResolution: 40000}
	// Inside optimal every shot hits: 1% wrecking at triple damage,
	// the rest averaging 0.995, so 1.01505 of paper.
	near(t, "inside optimal", gun.DamageAt(1000, nil), 101.505, 1e-9)
	// At optimal plus one falloff half the shots hit:
	// 0.03 + 0.49 * (0.255 + 0.49).
	near(t, "optimal + falloff", gun.DamageAt(8600, nil), 39.505, 1e-9)

	hml := WeaponReach{Kind: ReachMissile, DPS: 100, Range: 27950,
		ExplosionRadius: 140, ExplosionVelocity: 85, DamageReduction: 0.682}
	frig := &Target{Signature: 35, Velocity: 400}
	// min(1, 35/140, (35/140 * 85/400)^0.682) = 0.13509...
	near(t, "missile on a frigate", hml.DamageAt(10000, frig), 13.5094, 1e-3)
	near(t, "missile past flight range", hml.DamageAt(28000, nil), 0, 0)

	pod := WeaponReach{Kind: ReachBreacher, DPS: 200, HPPercent: 0.6, Range: 4000}
	near(t, "breacher, hit points unknown", pod.DamageAt(1000, frig), 200, 0)
	near(t, "breacher on 1,300 hit points", pod.DamageAt(1000, &Target{Signature: 35, Velocity: 400, HP: 1300}), 7.8, 1e-9)
	near(t, "breacher capped on 50,000 hit points", pod.DamageAt(1000, &Target{HP: 50000}), 200, 0)
	near(t, "breacher past flight range", pod.DamageAt(4001, nil), 0, 0)
}
