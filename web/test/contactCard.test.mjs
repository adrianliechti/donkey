import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cardDay, contactMatches, contactName, initials, typeLabel, vcardFileName } from '../src/contactCard.ts'

test('a card is called by its name, and its avatar by the name’s ends', () => {
  assert.equal(contactName({ name: '  ' }), 'No name')
  assert.equal(contactName({ name: 'Jane Doe' }), 'Jane Doe')
  assert.equal(initials('Jane Doe'), 'JD')
  assert.equal(initials('Jane van der Berg'), 'JB')
  assert.equal(initials('acme'), 'A')
  assert.equal(initials('jane.doe@example.com'), 'JC')
  assert.equal(initials('Élodie Ørsted'), 'ÉØ')
  assert.equal(initials('(Jane) Doe'), 'JD')
  assert.equal(initials(''), '')
  assert.equal(initials('— · —'), '')
})

test('a search finds a contact by name, org, email or phone digits', () => {
  const jane = { key: 'a.vcf', name: 'Jane Doe', org: 'Acme AG', email: 'jane@acme.test', phone: '+41 79 123 45 67' }
  assert.ok(contactMatches(jane, ''))
  assert.ok(contactMatches(jane, ' DOE '))
  assert.ok(contactMatches(jane, 'acme ag'))
  assert.ok(contactMatches(jane, 'jane@'))
  assert.ok(contactMatches(jane, '79 123'))
  assert.ok(contactMatches(jane, '1234567'))
  assert.ok(contactMatches(jane, '(79) 123-45'))
  assert.ok(!contactMatches(jane, '12'), 'two digits match too much to mean anything')
  assert.ok(!contactMatches(jane, 'bob'))
  assert.ok(!contactMatches({ key: 'b', name: 'Bob' }, '123'))
})

test('a value’s label says what it is for, in people’s words', () => {
  assert.equal(typeLabel({ value: 'x', types: ['cell', 'iphone'] }), 'mobile')
  assert.equal(typeLabel({ value: 'x', types: ['home', 'fax'] }), 'home fax')
  assert.equal(typeLabel({ value: 'x', types: ['x-assistant'] }), 'assistant')
  assert.equal(typeLabel({ value: 'x' }), '')
})

test('a birthday reads as a date, with or without its year', () => {
  assert.equal(cardDay('1980-05-01', 'en-GB'), '1 May 1980')
  assert.equal(cardDay('--05-01', 'en-GB'), '1 May')
  assert.equal(cardDay('--0229', 'en-GB'), '29 February')
  assert.equal(cardDay('1980-02-31', 'en-GB'), '1980-02-31')
  assert.equal(cardDay('circa 1980', 'en-GB'), 'circa 1980')
})

test('a download is named after the card, minus what file systems refuse', () => {
  assert.equal(vcardFileName('Jane Doe', 'contacts/1234.vcf'), 'Jane Doe.vcf')
  assert.equal(vcardFileName('AC/DC: "Live"', 'x'), 'AC DC Live.vcf')
  assert.equal(vcardFileName('', 'contacts/1234.vcf'), '1234.vcf')
  assert.equal(vcardFileName('', ''), 'contact.vcf')
})
