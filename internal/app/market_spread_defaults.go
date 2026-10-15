package app

// Core-owned defaults for reading the stored spread statistics
// (ListScannerOpportunities). They are shared data, not part of any
// page: the spread scanner page uses them as its starting filters and
// row cap, and other readers of the same stored statistics (the Trade
// Ideas module, through Deps.SpreadDefaults) use the same numbers.
// They live here, not in the scanner's file, so extracting the scanner
// into a module takes nothing the others read.

// scannerRowCap caps how many opportunities one read returns. The sort
// is by estimated daily profit, so the cap keeps the best trades and
// drops the long tail.
const scannerRowCap = 100

// defaultScannerMinSpreadPct is the floor the spread filter starts at:
// below this the gap rarely covers fees and effort.
const defaultScannerMinSpreadPct = 5.0
