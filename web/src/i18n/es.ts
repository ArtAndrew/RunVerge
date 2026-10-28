// SPDX-License-Identifier: Elastic-2.0
import {esCommon} from './es-common.ts'
import {esComponents} from './es-components.ts'
import {esAdmin} from './es-admin.ts'

export default {...esComponents, ...esAdmin, ...esCommon}
