//! `Cache<K, V>` trait for a stale-while-revalidate (SWR) cache.
//!
//! An SWR cache never blocks a reader on a fetch. A lookup returns
//! immediately with one of three outcomes: a fresh value, a stale value
//! (still usable, but due for a background revalidation), or a miss. The
//! trait only describes this contract; no implementation is provided yet.

use std::time::Duration;

/// Freshness of a value relative to the cache's configured windows.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Freshness {
    /// Within `Cache::fresh_for`; safe to use without revalidating.
    Fresh,
    /// Past `Cache::fresh_for` but within `Cache::fresh_for` + `Cache::stale_for`;
    /// usable, but the caller should trigger a background revalidation.
    Stale,
}

/// Result of a cache lookup.
pub enum Lookup<V> {
    /// No entry for the key, or it aged out past the stale window.
    Miss,
    /// An entry was found, along with its current freshness.
    Hit { value: V, freshness: Freshness },
}

/// A stale-while-revalidate cache from `K` to `V`.
///
/// Implementations own the storage and clock; this trait fixes only the
/// SWR contract:
///
/// - `get` never blocks on revalidation; it returns the best value on hand.
/// - An entry transitions `Fresh` -> `Stale` -> (evicted) as it ages past
///   `fresh_for` and then `fresh_for` + `stale_for`.
/// - Revalidation itself (deciding *when* and *how* to refetch a `Stale`
///   entry) is the caller's responsibility, not the cache's.
pub trait Cache<K, V> {
    /// Look up `key`. Never triggers a fetch or blocks on one.
    fn get(&self, key: &K) -> Lookup<V>;

    /// Insert or replace the value for `key`, resetting its age to zero
    /// (i.e. the entry becomes `Fresh`).
    fn set(&self, key: K, value: V);

    /// Remove any entry for `key`, so the next `get` is a `Miss`.
    fn invalidate(&self, key: &K);

    /// Duration after insertion during which an entry is `Fresh`.
    fn fresh_for(&self) -> Duration;

    /// Additional duration after `fresh_for` during which an entry is
    /// `Stale` rather than evicted outright.
    fn stale_for(&self) -> Duration;
}
