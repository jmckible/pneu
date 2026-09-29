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

test('parseCSV: quotes, escapes, CRLF, breaks inside quotes', () => {
  const text = 'Point,Note\r\nP1,"Iron pin, found"\r\nP3,"Fence ""old"" line"\r\nP4,"Two-line\r\nnote"\r\n';
  assert.deepEqual(V.parseCSV(text, ',', 100), {
    rows: [['Point', 'Note'], ['P1', 'Iron pin, found'], ['P3', 'Fence "old" line'], ['P4', 'Two-line\r\nnote']],
    truncated: false,
  });
  // LF, no trailing newline, empty fields, a quote mid-field is literal.
  assert.deepEqual(V.parseCSV('a,,c\nx"y,"",z', ',', 100).rows, [['a', '', 'c'], ['x"y', '', 'z']]);
  assert.deepEqual(V.parseCSV('a\tb,c\n1\t2', '\t', 100).rows, [['a', 'b,c'], ['1', '2']]);
  assert.deepEqual(V.parseCSV('', ',', 100).rows, []);
});

test('parseCSV: stops at maxRows', () => {
  assert.deepEqual(V.parseCSV('1\n2\n3\n', ',', 2), { rows: [['1'], ['2']], truncated: true });
  assert.deepEqual(V.parseCSV('1\n2\n', ',', 2), { rows: [['1'], ['2']], truncated: false });
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
