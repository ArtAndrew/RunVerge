// SPDX-License-Identifier: Elastic-2.0
import {deCommon} from './de-common.ts'
import {deComponents} from './de-components.ts'
import {deAdmin} from './de-admin.ts'

export default {...deComponents, ...deAdmin, ...deCommon}
