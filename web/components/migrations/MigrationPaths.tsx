"use client";

/** One supported route, and what moves along it. */
const PATHS: { from: string; to: string; moves: string; follows: string }[] = [
  { from: "PostgreSQL 10+", to: "newer PostgreSQL", moves: "Tables, data, types, sequences, functions, views, indexes, constraints, triggers", follows: "logical replication" },
  { from: "MySQL 5.6+", to: "newer MySQL", moves: "Tables, data, views, routines, triggers, events", follows: "the binary log" },
  { from: "Oracle 12c+", to: "newer Oracle", moves: "Tables, data, sequences, indexes, constraints, views, synonyms, PL/SQL, triggers", follows: "LogMiner" },
  { from: "MySQL", to: "PostgreSQL", moves: "Tables, data, keys, indexes, foreign keys; types translated", follows: "the binary log" },
  { from: "PostgreSQL", to: "MySQL 8.0+", moves: "Tables, data, keys, indexes, foreign keys; types translated", follows: "logical replication" },
  { from: "Oracle 12c+", to: "PostgreSQL", moves: "Tables, data, keys, sequences, identity columns, indexes, checks, foreign keys; types translated", follows: "LogMiner" },
  { from: "Oracle 12c+", to: "MySQL 8.0+", moves: "Tables, data, keys, identity columns, indexes, checks, foreign keys; types translated", follows: "LogMiner" },
  { from: "MongoDB 4.0+", to: "PostgreSQL", moves: "Each collection as a table of jsonb documents, with chosen fields as typed columns", follows: "the change stream" },
];

/**
 * A reference card for the Migrations page: every route, what it carries
 * and what it reads to follow changes. Every route also runs offline, in
 * which case nothing is followed and the source needs no setup.
 */
export default function MigrationPaths() {
  return (
    <div className="card">
      <div className="card-head"><h2>Supported paths</h2></div>
      <div className="card-body flush table-wrap">
        <table className="data">
          <thead>
            <tr><th>From</th><th>To</th><th>What moves</th><th>Online, it follows</th></tr>
          </thead>
          <tbody>
            {PATHS.map((p) => (
              <tr key={p.from + p.to}>
                <td><strong>{p.from}</strong></td>
                <td>{p.to}</td>
                <td>{p.moves}</td>
                <td>{p.follows}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="card-body">
        <p className="hint">
          Every path runs three ways: <strong>online</strong>, following changes until you cut over; <strong>offline</strong>, as a
          one-time copy that needs no replication set up on the source; or as a <strong>continuous sync</strong> that never cuts
          over. Moving between engines does not translate views, routines or triggers. Preflight lists everything that will
          not be carried over, and everything the target already has, before anything is changed.
        </p>
      </div>
    </div>
  );
}
