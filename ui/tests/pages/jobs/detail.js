import {
  attribute,
  create,
  collection,
  clickable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

export default create({
  visit: visitable('/jobs/:id'),

  tabs: collection('[data-test-tab]', {
    id: attribute('data-test-tab'),
    visit: clickable('a'),
  }),

  tabFor(id) {
    return this.tabs.toArray().findBy('id', id);
  },

  stats: collection('[data-test-job-stat]', {
    id: attribute('data-test-job-stat'),
    text: text(),
  }),

  statFor(id) {
    return this.stats.toArray().findBy('id', id);
  },

  error: {
    isPresent: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },
});
