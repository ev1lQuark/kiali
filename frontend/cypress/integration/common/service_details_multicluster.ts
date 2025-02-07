import { Then, And } from '@badeball/cypress-cucumber-preprocessor';
import { clusterParameterExists } from './navigation';

function openTab(tab: string) {
  cy.get('.pf-v5-c-tabs__list').should('be.visible').contains(tab).click();
}

Then('sd::user sees {string} details information for the remote service {string}', (name: string, version: string) => {
  cy.get('#ServiceDescriptionCard').within(() => {
    cy.get('#pfbadge-S').parent().parent().parent().contains(name); // Service
    cy.get('#pfbadge-A').parent().parent().parent().contains(name); // App
    cy.get('#pfbadge-W')
      .parent()
      .parent()
      .parent()
      .contains(name + '-' + version); // Workload
  });
});

Then('user does not see a minigraph',() =>{
  cy.get('#MiniGraphCard').find('h5').contains('Empty Graph');
});

Then('sd::user sees inbound and outbound traffic information for the remote service', () => {
  openTab('Traffic');
  cy.get('.pf-v5-c-card__body').within(() => {
    cy.contains('Inbound Traffic');
    cy.contains('No Inbound Traffic').should('not.exist');
    cy.contains('Outbound Traffic');
    cy.contains('No Inbound Traffic').should('not.exist');
    cy.get('table.pf-v5-c-table.pf-m-grid-md').should('exist');
  });
});

And('links in the {string} description card should contain a reference to a {string} cluster',(type:string, cluster:string) => {
  cy.get(`#${type}DescriptionCard`).within(() => {
    cy.get('a').each(($el, index, $list) =>{
      cy.wrap($el)
      .should('have.attr', 'href')
      .and('include', `clusterName=${cluster}`);
    });
  })
});

And('cluster badge for {string} cluster should be visible in the {string} description card',(cluster:string, type:string) => {
  cy.get(`div #${type}DescriptionCard`).find('#pfbadge-C').parent().parent().contains(cluster);
});
