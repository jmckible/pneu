// node --test web/viewer.test.js — the pure parts of the attachment viewer
// (web/static/viewer.js). Lives outside web/static so the embed doesn't serve it.
'use strict';
process.env.TZ = 'UTC'; // before any Date: UTC times render in "local" time
const test = require('node:test');
const assert = require('node:assert/strict');
const V = require('./static/viewer.js');

test('charset: the label, else utf-8', () => {
  assert.equal(V.charset('text/plain; charset=ISO-8859-1'), 'ISO-8859-1');
  assert.equal(V.charset('text/csv;charset="windows-1252"'), 'windows-1252');
  assert.equal(V.charset('application/json'), 'utf-8');
  assert.equal(V.charset(null), 'utf-8');
});

test('decode: by charset, unknown labels fall back to utf-8', () => {
  assert.equal(V.decode(new Uint8Array([0x63, 0x61, 0x66, 0xe9]), 'text/plain; charset=iso-8859-1'), 'café');
  assert.equal(V.decode(new TextEncoder().encode('café'), 'text/plain; charset=x-no-such-thing'), 'café');
  assert.equal(V.decode(new TextEncoder().encode('﻿a,b'), 'text/csv'), 'a,b');
});

// parse with caps too high to matter.
const parse = (text, delim, maxRows) => V.parseCSV(text, delim, maxRows, 1000, 1e6);

test('parseCSV: quotes, escapes, CRLF, breaks inside quotes', () => {
  const text = 'Point,Note\r\nP1,"Iron pin, found"\r\nP3,"Fence ""old"" line"\r\nP4,"Two-line\r\nnote"\r\n';
  assert.deepEqual(parse(text, ',', 100), {
    rows: [['Point', 'Note'], ['P1', 'Iron pin, found'], ['P3', 'Fence "old" line'], ['P4', 'Two-line\r\nnote']],
    rowsCut: false,
    colsCut: false,
  });
  // LF, no trailing newline, empty fields, a quote mid-field is literal.
  assert.deepEqual(parse('a,,c\nx"y,"",z', ',', 100).rows, [['a', '', 'c'], ['x"y', '', 'z']]);
  assert.deepEqual(parse('a\tb,c\n1\t2', '\t', 100).rows, [['a', 'b,c'], ['1', '2']]);
  assert.deepEqual(parse('', ',', 100).rows, []);
  assert.deepEqual(parse('a,\n', ',', 100).rows, [['a', '']]);
});

test('parseCSV: stops at maxRows', () => {
  assert.deepEqual(parse('1\n2\n3\n', ',', 2), { rows: [['1'], ['2']], rowsCut: true, colsCut: false });
  assert.deepEqual(parse('1\n2\n', ',', 2), { rows: [['1'], ['2']], rowsCut: false, colsCut: false });
});

test('parseCSV: keeps maxCols of each row, and parses the rest', () => {
  // The skipped fields still quote: the comma and newline inside one don't
  // start a field or a row.
  assert.deepEqual(V.parseCSV('a,b,c,"d,\ne"\n1,2\n', ',', 100, 2, 100), {
    rows: [['a', 'b'], ['1', '2']], rowsCut: false, colsCut: true,
  });
  assert.deepEqual(V.parseCSV('a,b\n1,2', ',', 100, 2, 100).colsCut, false);
  // A line of 100,000 commas is maxCols cells, not 100,001.
  const wide = V.parseCSV(','.repeat(100000) + '\nx', ',', V.CSV_ROWS + 1, V.CSV_COLS, V.CSV_CELLS);
  assert.equal(wide.rows[0].length, V.CSV_COLS);
  assert.deepEqual(wide.rows[1], ['x']);
  assert.equal(wide.colsCut, true);
  assert.equal(wide.rowsCut, false);
});

test('parseCSV: stops at maxCells', () => {
  // Three rows of three: the budget of 7 ends in the third row.
  assert.deepEqual(V.parseCSV('1,2,3\n4,5,6\n7,8,9\n', ',', 100, 10, 7), {
    rows: [['1', '2', '3'], ['4', '5', '6'], ['7']], rowsCut: true, colsCut: false,
  });
  // Spent exactly at a row's end, with more to come.
  assert.deepEqual(V.parseCSV('1,2\n3,4\n5,6', ',', 100, 10, 4), {
    rows: [['1', '2'], ['3', '4']], rowsCut: true, colsCut: false,
  });
  assert.deepEqual(V.parseCSV('1,2\n3,4\n', ',', 100, 10, 4).rowsCut, false);
  // The real caps: no more than CSV_CELLS cells, however the file is shaped.
  const line = Array(V.CSV_COLS).fill('x').join(',') + '\n';
  const big = V.parseCSV(line.repeat(V.CSV_ROWS), ',', V.CSV_ROWS + 1, V.CSV_COLS, V.CSV_CELLS);
  assert.equal(big.rows.reduce((n, r) => n + r.length, 0), V.CSV_CELLS);
  assert.equal(big.rows.length, V.CSV_CELLS / V.CSV_COLS);
  assert.equal(big.rowsCut, true);
});

test('icsFields: folded lines, escapes, TZID, organizer', () => {
  const ics = [
    'BEGIN:VCALENDAR', 'METHOD:REQUEST',
    'BEGIN:VEVENT',
    'DTSTART;TZID=America/Los_Angeles:20261003T093000',
    'DTEND;TZID=America/Los_Angeles:20261003T110000',
    'SUMMARY:Site visit\\, lot 14',
    'LOCATION:Gate\\; by the mailbox',
    'ORGANIZER;CN="Whitaker: Dana":mailto:dana@larkspur-survey.example',
    'DESCRIPTION:Walk the line.\\nBring boots\\, it may be ',
    ' muddy.',
    'BEGIN:VALARM', 'DESCRIPTION:Reminder', 'END:VALARM',
    'END:VEVENT', 'END:VCALENDAR', '',
  ].join('\r\n');
  assert.deepEqual(V.icsFields(ics), [
    ['Event', 'Site visit, lot 14'],
    ['Starts', 'Sat, Oct 3, 2026, 9:30 AM (America/Los_Angeles)'],
    ['Ends', 'Sat, Oct 3, 2026, 11:00 AM (America/Los_Angeles)'],
    ['Where', 'Gate; by the mailbox'],
    ['Organizer', 'Whitaker: Dana <dana@larkspur-survey.example>'],
    ['Details', 'Walk the line.\nBring boots, it may be muddy.'],
  ]);
});

test('icsFields: UTC and all-day', () => {
  const utc = 'BEGIN:VEVENT\nSUMMARY:Call\nDTSTART:20260105T233000Z\nEND:VEVENT\n';
  assert.deepEqual(V.icsFields(utc), [['Event', 'Call'], ['Starts', 'Mon, Jan 5, 2026, 11:30 PM']]);
  const day = 'BEGIN:VEVENT\nDTSTART;VALUE=DATE:20261003\nDTEND;VALUE=DATE:20261004\nEND:VEVENT';
  assert.deepEqual(V.icsFields(day), [['Date', 'Sat, Oct 3, 2026']]);
  const days = 'BEGIN:VEVENT\nDTSTART;VALUE=DATE:20261003\nDTEND;VALUE=DATE:20261006\nEND:VEVENT';
  assert.deepEqual(V.icsFields(days), [['Date', 'Sat, Oct 3, 2026'], ['Until', 'Mon, Oct 5, 2026']]);
  assert.equal(V.icsFields('BEGIN:VCALENDAR\nEND:VCALENDAR'), null);
});

test('size', () => {
  assert.equal(V.size(900), '900 B');
  assert.equal(V.size(2048), '2.0 KB');
  assert.equal(V.size(3.5 * 1024 * 1024), '3.5 MB');
});
