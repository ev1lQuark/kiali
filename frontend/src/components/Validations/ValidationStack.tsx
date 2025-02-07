import React from 'react';
import { ObjectCheck, ValidationTypes } from '../../types/IstioObjects';
import { Validation } from './Validation';
import { highestSeverity } from '../../types/ServiceInfo';
import { Stack, StackItem, Text, TextVariants } from '@patternfly/react-core';
import { kialiStyle } from 'styles/StyleUtils';
import { PFColors } from '../Pf/PfColors';

type ValidationStackProps = {
  checks?: ObjectCheck[];
};

const colorStyle = kialiStyle({ color: PFColors.White });
const titleStyle = kialiStyle({ color: PFColors.White, fontSize: '1.1rem' });

export const ValidationStack: React.FC<ValidationStackProps> = (props: ValidationStackProps) => {
  const validationList = (): React.ReactNode[] => {
    return (props.checks ?? []).map((check, index) => {
      return (
        <StackItem key={`validation-check-item-${index}`} className={colorStyle}>
          <Validation
            key={`validation-check-${index}`}
            severity={check.severity}
            message={`${check.code ? `${check.code} ` : ''}${check.message}`}
          />
        </StackItem>
      );
    });
  };

  const severity = highestSeverity(props.checks ?? []);
  const isValid = severity === ValidationTypes.Correct;

  if (!isValid) {
    return (
      <Stack>
        <StackItem className={titleStyle}>
          <Text component={TextVariants.h1}>Istio validations</Text>
        </StackItem>

        {validationList()}
      </Stack>
    );
  } else {
    return null;
  }
};
