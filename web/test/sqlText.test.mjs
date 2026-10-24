import assert from 'node:assert/strict'
import { test } from 'node:test'
import { columnWidth, isNumericType, jsonOf, onlyReads, statementAt, statements, toCsv } from '../src/sqlText.ts'

test('semicolons inside strings, names, comments and dollar quotes end nothing', () => {
  const script = `select 'a;b', "x;y" from t; -- one; two
insert into t values (E'it\\'s;', 'it''s;');
/* a; /* nested; */ still; */ select 1;
create function f() returns int as $body$ begin; return 1; end $body$ language plpgsql;
select $1`
  assert.deepEqual(statements(script, 'postgres'), [
    `select 'a;b', "x;y" from t`,
    `-- one; two\ninsert into t values (E'it\\'s;', 'it''s;')`,
    `/* a; /* nested; */ still; */ select 1`,
    `create function f() returns int as $body$ begin; return 1; end $body$ language plpgsql`,
    'select $1',
  ])
})

test('MySQL escapes with backslashes, quotes names with backticks and comments with #', () => {
  assert.deepEqual(statements("select 'a\\';b', `c;d` from t; # gone;\nselect 2;", 'mysql'), [
    "select 'a\\';b', `c;d` from t",
    '# gone;\nselect 2',
  ])
  // In postgres a backslash is just a character outside E'' strings.
  assert.deepEqual(statements("select 'a\\'; select 2", 'postgres'), ["select 'a\\'", 'select 2'])
})

test('stretches of comments and blank statements are not statements', () => {
  assert.deepEqual(statements(';;  -- nothing here\n ; /* nor here */', 'postgres'), [])
  assert.deepEqual(statements('select 1;\n\n-- trailing note', 'trino'), ['select 1'])
})

test('the statement at the cursor is the one it is in, or the one it just left', () => {
  const text = 'select 1;\n  select 2;\n\nselect 3'
  const at = (cursor) => {
    const r = statementAt(text, cursor, 'postgres')
    return r && text.slice(r.start, r.end)
  }
  assert.equal(at(0), 'select 1')
  assert.equal(at(9), 'select 1') // right after the semicolon
  assert.equal(at(14), 'select 2')
  assert.equal(at(text.length), 'select 3')
  assert.equal(statementAt('  ', 1, 'postgres'), null)
  assert.equal(statementAt('\n\nselect 1', 0, 'postgres')?.start, 2)
})

test('CSV quotes what needs quoting and leaves null empty', () => {
  assert.equal(
    toCsv(['id', 'note'], [['1', 'plain'], ['2', 'a, "quoted"\nline'], ['3', null], ['4', ' padded']]),
    'id,note\r\n1,plain\r\n2,"a, ""quoted""\nline"\r\n3,\r\n4," padded"\r\n',
  )
})

test('value shapes', () => {
  assert.deepEqual(jsonOf('{"a": [1]}'), { a: [1] })
  assert.equal(jsonOf('{not json}'), undefined)
  assert.equal(jsonOf('42'), undefined)
  assert.equal(jsonOf(null), undefined)
  for (const type of ['integer', 'bigint', 'numeric(10,2)', 'double precision', 'int(11) unsigned', 'decimal(20,6)', 'real']) {
    assert.ok(isNumericType(type), type)
  }
  for (const type of ['text', 'interval', 'point', 'timestamp with time zone', 'json', undefined]) {
    assert.ok(!isNumericType(type), String(type))
  }
  assert.ok(columnWidth('id', 'bigint', ['1']) < columnWidth('description', 'text', ['x'.repeat(40)]))
  assert.ok(columnWidth('blob', 'text', ['x'.repeat(5000)]) <= 380)
})

test('only plain reads leave the cached tables alone', () => {
  for (const sql of ['select 1', '  -- why\nSELECT * from t', 'show tables', 'explain select 1', '/* c */ values (1)']) {
    assert.ok(onlyReads(sql), sql)
  }
  for (const sql of ['update t set a = 1', 'select * into copy from t', 'with d as (delete from t returning *) select * from d', 'create table x ()']) {
    assert.ok(!onlyReads(sql), sql)
  }
})
